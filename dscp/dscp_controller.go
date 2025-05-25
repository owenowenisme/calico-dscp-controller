package dscp

import (
	"bytes"
	"context"
	"fmt"
	"gopkg.in/yaml.v3"
	"reflect"
	"strings"
	"time"

	appsv1informer "k8s.io/client-go/informers/apps/v1"
	corev1informer "k8s.io/client-go/informers/core/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	appsv1lister "k8s.io/client-go/listers/apps/v1"
	corev1lister "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/workqueue"
)

const (
	appName         = "dscp"
	controllerName  = "dscp-controller"
	configNamespace = "kube-system"
	configName      = "k8s-dscp-config"
	daemonSetName   = "k8s-dscp"

	// DSCP ConfigMap yaml data
	configMapYamlData = "k8s_dscp_config.yaml"
)

type NamespaceDscp struct {
	Name string `yaml:"name"`
	DSCP string `yaml:"dscp"`
}

type DscpConfig struct {
	ContainerImage   string              `yaml:"container_image"`
	QueueDscpMap     map[string][]string `yaml:"queue_dscp_map"`
	NamespaceDscpMap []NamespaceDscp     `yaml:"namespace_dscp_map"`
}

// Controller is the controller implementation for DSCP resources
type Controller struct {
	// kubeclientset is a standard kubernetes clientset
	kubeclientset kubernetes.Interface

	configMapLister  corev1lister.ConfigMapLister
	configMapsSynced cache.InformerSynced

	daemonSetLister  appsv1lister.DaemonSetLister
	daemonSetsSynced cache.InformerSynced

	podLister  corev1lister.PodLister
	podsSynced cache.InformerSynced

	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens.
	workqueue workqueue.RateLimitingInterface

	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder
}

// NewDscpController returns a new DSCP controller
func NewDscpController(
	kubeclientset kubernetes.Interface,
	configMapInformer corev1informer.ConfigMapInformer,
	daemonSetInformer appsv1informer.DaemonSetInformer,
	podInformer corev1informer.PodInformer) *Controller {

	klog.V(4).Info("Creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerName})

	controller := &Controller{
		kubeclientset:    kubeclientset,
		configMapLister:  configMapInformer.Lister(),
		configMapsSynced: configMapInformer.Informer().HasSynced,
		daemonSetLister:  daemonSetInformer.Lister(),
		daemonSetsSynced: daemonSetInformer.Informer().HasSynced,
		podLister:        podInformer.Lister(),
		podsSynced:       podInformer.Informer().HasSynced,
		workqueue:        workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), appName),
		recorder:         recorder,
	}

	klog.Info("Setting up event handlers")

	// Add event handlers
	configMapInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueConfigMap,
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldCm := oldObj.(*corev1.ConfigMap)
			newCm := newObj.(*corev1.ConfigMap)

			if reflect.DeepEqual(oldCm.Data, newCm.Data) {
				return // If the ConfigMap content does not change, don't enqueue
			}
			controller.enqueueConfigMap(newObj)
		},
		DeleteFunc: controller.enqueueConfigMap,
	})

	daemonSetInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueDaemonSet,
		UpdateFunc: func(oldObj, newObj interface{}) {
			controller.enqueueDaemonSet(newObj)
		},
		DeleteFunc: controller.enqueueDaemonSet,
	})

	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.handlePodChange,
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPod := oldObj.(*corev1.Pod)
			newPod := newObj.(*corev1.Pod)
			if oldPod.Status.PodIP == "" && newPod.Status.PodIP != "" {
				controller.handlePodChange(newObj)
			}
		},
		DeleteFunc: controller.handlePodChange,
	})

	return controller
}

// Start initiates the controller
func (c *Controller) Start(ctx context.Context) error {
	return c.Run(1, ctx.Done())
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers.
func (c *Controller) Run(workers int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	klog.Info("Starting DSCP controller")

	// Wait for the caches to be synced before starting workers
	klog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.configMapsSynced, c.daemonSetsSynced, c.podsSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	klog.Info("Starting workers")
	// Launch workers to process resources
	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	klog.Info("Started workers")
	<-stopCh
	klog.Info("Shutting down workers")

	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function to read and process a message on the workqueue.
func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the reconcile.
func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item.
		defer c.workqueue.Done(obj)

		var key string
		var ok bool

		// We expect strings to come off the workqueue. These are of the
		// form namespace/name.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}

		// Run the reconcile, passing it the namespace/name string of the resource
		if err := c.reconcile(key); err != nil {
			// Put the item back on the workqueue to handle any transient errors.
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error reconciling '%s': %s, requeuing", key, err.Error())
		}

		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		klog.Infof("Successfully reconciled '%s'", key)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}

// reconcile is the core reconciliation loop that:
// 1. Determines the current state
// 2. Determines the desired state
// 3. Reconciles the two
func (c *Controller) reconcile(key string) error {
	klog.V(4).Infof("Reconciling key: %s", key)
	// Parse the namespace and name from the key
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("invalid resource key: %s", key)
	}

	// Only process the DSCP ConfigMap
	if namespace != configNamespace || name != configName {
		// This could be a DaemonSet update event, check if we need to reconcile the ConfigMap
		if namespace == configNamespace && strings.HasPrefix(name, daemonSetName) {
			// Get the ConfigMap to reconcile
			_, err := c.configMapLister.ConfigMaps(configNamespace).Get(configName)
			if err != nil {
				if errors.IsNotFound(err) {
					// ConfigMap doesn't exist, handle DaemonSet deletion
					return c.reconcileDeletion(configNamespace)
				}
				return err
			}
			// Use the ConfigMap key for reconciliation
			return c.reconcile(configNamespace + "/" + configName)
		}
		// Not our resource to reconcile
		return nil
	}

	// Get the DSCP ConfigMap
	configMap, err := c.configMapLister.ConfigMaps(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			// ConfigMap is deleted - clean up resources
			return c.reconcileDeletion(namespace)
		}
		return err
	}

	// Parse the DSCP ConfigMap
	dscpConfig, err := c.parseDscpConfig(configMap)
	if err != nil {
		return fmt.Errorf("failed to parse DSCP config: %v", err)
	}

	// Reconcile the DaemonSet
	if err := c.reconcileDaemonSet(namespace, dscpConfig); err != nil {
		return fmt.Errorf("failed to reconcile DaemonSet: %v", err)
	}

	// Reconcile iptables for
	ds, err := c.daemonSetLister.DaemonSets(configNamespace).Get(daemonSetName)
	if err != nil {
		return fmt.Errorf("failed to get DaemonSet: %v", err)
	}
	labelSelector, err := metav1.LabelSelectorAsSelector(ds.Spec.Selector)
	pods, err := c.podLister.Pods(ds.Namespace).List(labelSelector)
	if err := c.reconcileIptables(dscpConfig, pods); err != nil {
		return fmt.Errorf("failed to reconcile iptables: %v", err)
	}

	return nil
}

// reconcileDeletion handles cleanup when the ConfigMap is deleted
func (c *Controller) reconcileDeletion(namespace string) error {
	klog.Infof("Reconciling deletion in namespace %s", namespace)

	// Check if the DaemonSet exists
	_, err := c.daemonSetLister.DaemonSets(namespace).Get(daemonSetName)
	if err != nil {
		if errors.IsNotFound(err) {
			// DaemonSet is already gone, nothing to do
			klog.Infof("DaemonSet %s already deleted", daemonSetName)
			return nil
		}
		return err
	}

	// Delete the DaemonSet if it exists
	err = c.kubeclientset.AppsV1().DaemonSets(namespace).Delete(
		context.TODO(),
		daemonSetName,
		metav1.DeleteOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to delete DaemonSet %s: %v", daemonSetName, err)
	}

	klog.Infof("Successfully deleted DaemonSet %s", daemonSetName)
	return nil
}

// parseDscpConfig parses the DSCP ConfigMap into a DscpConfig struct
func (c *Controller) parseDscpConfig(configMap *corev1.ConfigMap) (DscpConfig, error) {
	var dscpConfig DscpConfig

	yamlData, exists := configMap.Data[configMapYamlData]
	if !exists {
		return dscpConfig, fmt.Errorf("ConfigMap %s/%s does not contain key %s",
			configMap.Namespace, configMap.Name, configMapYamlData)
	}

	err := yaml.Unmarshal([]byte(yamlData), &dscpConfig)
	if err != nil {
		return dscpConfig, fmt.Errorf("failed to unmarshal DSCP config: %v", err)
	}

	return dscpConfig, nil
}

// reconcileDaemonSet ensures the DaemonSet matches the desired state
func (c *Controller) reconcileDaemonSet(namespace string, dscpConfig DscpConfig) error {
	// Check if DaemonSet exists
	existingDS, err := c.daemonSetLister.DaemonSets(namespace).Get(daemonSetName)
	// Generate the desired DaemonSet
	timestamp := time.Now().Format(time.RFC3339)
	desiredDS := c.generateDaemonSet(dscpConfig, timestamp)

	if err != nil {
		if errors.IsNotFound(err) {
			// Create the DaemonSet if it doesn't exist
			_, err := c.kubeclientset.AppsV1().DaemonSets(namespace).Create(
				context.TODO(),
				desiredDS,
				metav1.CreateOptions{},
			)
			if err != nil {
				return fmt.Errorf("failed to create DaemonSet: %v", err)
			}
			klog.Infof("Created DaemonSet %s in namespace %s", daemonSetName, namespace)
			return nil
		}
		return err
	}

	// Check if update is needed
	if c.daemonSetNeedsUpdate(existingDS, desiredDS) {
		// Use existing DS as base and update necessary fields
		updatedDS := existingDS.DeepCopy()
		updatedDS.Spec.Template.Spec.Containers = desiredDS.Spec.Template.Spec.Containers
		updatedDS.Annotations = desiredDS.Annotations

		_, err := c.kubeclientset.AppsV1().DaemonSets(namespace).Update(
			context.TODO(),
			updatedDS,
			metav1.UpdateOptions{},
		)
		if err != nil {
			return fmt.Errorf("failed to update DaemonSet: %v", err)
		}
		klog.Infof("Updated DaemonSet %s in namespace %s", daemonSetName, namespace)
	}

	return nil
}

// daemonSetNeedsUpdate determines if the DaemonSet needs an update
func (c *Controller) daemonSetNeedsUpdate(existing, desired *appsv1.DaemonSet) bool {
	// Compare container image and args
	if len(existing.Spec.Template.Spec.Containers) != len(desired.Spec.Template.Spec.Containers) {
		klog.Infof("DaemonSet needs update: container count mismatch %d != %d", len(existing.Spec.Template.Spec.Containers), len(desired.Spec.Template.Spec.Containers))
		return true
	}

	existingContainer := existing.Spec.Template.Spec.Containers[0]
	desiredContainer := desired.Spec.Template.Spec.Containers[0]

	if existingContainer.Image != desiredContainer.Image {
		klog.Infof("DaemonSet needs update: container image mismatch %s != %s", existingContainer.Image, desiredContainer.Image)
		return true
	}

	if !reflect.DeepEqual(existingContainer.Args, desiredContainer.Args) {
		klog.Infof("DaemonSet needs update: container args mismatch")
		return true
	}

	return false
}

// reconcileIptables ensures the iptables rules are up to date
func (c *Controller) reconcileIptables(dscpConfig DscpConfig, pods []*corev1.Pod) error {
	dscpIpMap := c.buildDscpIpMap(dscpConfig)
	commands := generateIptablesDscpCommand(dscpIpMap, true)
	for _, pod := range pods {
		if !isPodReady(pod) {
			klog.V(4).Infof("Skipping pod %s/%s as it's not ready", pod.Namespace, pod.Name)
			continue
		}

		err := c.executeCommandInPods(commands)

		if err != nil {
			return fmt.Errorf("failed to execute iptables commands: %v", err)
		}
	}
	return nil
}

// buildDscpIpMap creates a map of DSCP values to IPs from config
func (c *Controller) buildDscpIpMap(dscpConfig DscpConfig) map[string][]string {
	dscpIpMap := make(map[string][]string)

	for _, ns := range dscpConfig.NamespaceDscpMap {
		klog.V(4).Infof("Processing namespace: %s, DSCP: %s", ns.Name, ns.DSCP)
		if _, ok := dscpIpMap[ns.DSCP]; !ok {
			dscpIpMap[ns.DSCP] = make([]string, 0)
		}
		// Get pods in this namespace
		podList, err := c.podLister.Pods(ns.Name).List(labels.Everything())
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("error listing pods in namespace %s: %v", ns.Name, err))
			continue
		}

		// Add pod IPs to the map
		for _, pod := range podList {
			if !pod.Spec.HostNetwork && pod.Status.PodIP != "" {
				dscpIpMap[ns.DSCP] = append(dscpIpMap[ns.DSCP], pod.Status.PodIP+"/32")
				klog.V(4).Infof("Added pod %s/%s with IP %s", pod.Namespace, pod.Name, pod.Status.PodIP)
			}
		}
	}

	return dscpIpMap
}

// generateDaemonSet creates a DaemonSet object from the DSCP config
func (c *Controller) generateDaemonSet(dscpConfig DscpConfig, timestamp string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      daemonSetName,
			Namespace: configNamespace,
			Labels: map[string]string{
				"created-by":      controllerName,
				"configmap-owner": configName,
			},
			Annotations: map[string]string{
				"create-time": timestamp,
			},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": appName},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":             appName,
						"daemonset-owner": daemonSetName,
					},
				},
				Spec: corev1.PodSpec{
					HostNetwork: true,
					Containers: []corev1.Container{
						{
							Name:            "dscp-container",
							Image:           dscpConfig.ContainerImage,
							Command:         []string{"sleep", "infinity"},
							ImagePullPolicy: corev1.PullAlways,
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add:  []corev1.Capability{"NET_ADMIN"},
									Drop: []corev1.Capability{"KILL"},
								},
							},
						},
					},
				},
			},
		},
	}
}

// generateIptablesDscpCommand creates the iptables commands for DSCP marking
func generateIptablesDscpCommand(dscpIpMap map[string][]string, updateFlag bool) []string {
	commands := make([]string, 0)
	commands = append(commands, "iptables -t mangle -F")

	for dscp, podIps := range dscpIpMap {	
		for _, ip := range podIps {
			markPacket := fmt.Sprintf("iptables -t mangle -A POSTROUTING -s %s -m comment --comment \"cali-dscp: %s\" -j MARK --set-mark %s", ip, dscp, dscp)
			commands = append(commands, markPacket)
		}
		setDscp := fmt.Sprintf("iptables -t mangle -A POSTROUTING -m mark --mark %s -m comment --comment \"cali-dscp: %s\" -j DSCP --set-dscp %s", dscp, dscp, dscp)
		commands = append(commands, setDscp)
	}

	if !updateFlag {
		// For initial DaemonSet pod command
		commands = append(commands, "sleep infinity")
		fullCommandStr := strings.Join(commands, " && ")
		return []string{fullCommandStr}
	} else {
		// For updates via exec
		fullCommandStr := strings.Join(commands, " && ")
		return []string{"sh", "-c", fullCommandStr}
	}
}

// executeCommandInPods executes commands in all DSCP pods
func (c *Controller) executeCommandInPods(command []string) error {
	config, err := getKubeConfig()
	if err != nil {
		return fmt.Errorf("failed to get kubernetes config: %v", err)
	}

	selector := labels.SelectorFromSet(labels.Set(map[string]string{
		"app":             appName,
		"daemonset-owner": daemonSetName,
	}))

	pods, err := c.podLister.Pods(configNamespace).List(selector)
	if err != nil {
		return fmt.Errorf("failed to list DSCP pods: %v", err)
	}

	if len(pods) == 0 {
		klog.Warningf("No DSCP pods found to execute command")
		return nil
	}

	// Execute command in each pod
	for _, pod := range pods {
		req := c.kubeclientset.CoreV1().RESTClient().Post().
			Resource("pods").
			Name(pod.Name).
			Namespace(pod.Namespace).
			SubResource("exec")

		req.VersionedParams(&corev1.PodExecOptions{
			Command: command,
			Stdout:  true,
			Stderr:  true,
		}, scheme.ParameterCodec)

		// Create executor
		exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
		if err != nil {
			return fmt.Errorf("failed to create SPDY executor for pod %s: %v", pod.Name, err)
		}

		// Execute command
		var stdout, stderr bytes.Buffer
		err = exec.Stream(remotecommand.StreamOptions{
			Stdout: &stdout,
			Stderr: &stderr,
		})

		if err != nil {
			return fmt.Errorf("failed to execute command in pod %s: %v, stderr: %s",
				pod.Name, err, stderr.String())
		}

		if stderr.Len() > 0 {
			klog.Warningf("Command stderr in pod %s: %s", pod.Name, stderr.String())
		}

		klog.Infof("Command executed successfully in pod %s: %s", pod.Name, stdout.String())
	}

	return nil
}

// handlePodChange handles pod changes (add/update/delete)
func (c *Controller) handlePodChange(obj interface{}) {
	var pod *corev1.Pod
	var isDelete bool

	switch p := obj.(type) {
	case *corev1.Pod:
		pod = p
	case cache.DeletedFinalStateUnknown:
		var ok bool
		pod, ok = p.Obj.(*corev1.Pod)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding pod tombstone, invalid type"))
			return
		}
		isDelete = true
	default:
		utilruntime.HandleError(fmt.Errorf("unexpected object type: %T", obj))
		return
	}

	// Skip DSCP pods
	if isDscpPod(pod) {
		klog.V(4).Infof("Skipping DSCP pod event: %s/%s", pod.Namespace, pod.Name)
		return
	}

	// If it's a delete event or the pod has an IP address, trigger reconciliation
	if isDelete || (!pod.Spec.HostNetwork && pod.Status.PodIP != "") {
		key := configNamespace + "/" + configName
		c.workqueue.Add(key)
	}
}

// isDscpPod returns true if the pod is part of the DSCP DaemonSet
func isDscpPod(pod *corev1.Pod) bool {
	appVal, appOk := pod.Labels["app"]
	ownerVal, ownerOk := pod.Labels["daemonset-owner"]

	return appOk && ownerOk && appVal == appName && ownerVal == daemonSetName
}

// enqueueConfigMap adds a ConfigMap to the work queue if it's the DSCP ConfigMap
func (c *Controller) enqueueConfigMap(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}

	// Only enqueue the DSCP ConfigMap
	dscpConfigMapKey := configNamespace + "/" + configName
	if key != dscpConfigMapKey {
		klog.V(4).Infof("Ignoring non-DSCP ConfigMap: %s", key)
		return
	}
	c.workqueue.Add(key)
}

// enqueueDaemonSet adds a DaemonSet to the work queue if it's the DSCP DaemonSet
func (c *Controller) enqueueDaemonSet(obj interface{}) {
	var key string
	var err error

	key, err = cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}

	// Extract namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}

	// Only process our DaemonSet
	if namespace != configNamespace || name != daemonSetName {
		return
	}

	// Trigger reconciliation of the ConfigMap
	configMapKey := configNamespace + "/" + configName
	c.workqueue.Add(configMapKey)
}
