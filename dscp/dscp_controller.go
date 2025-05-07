package dscp

import (
	"bytes"
	"context"
	"fmt"
	"gopkg.in/yaml.v3"
	"k8s.io/client-go/rest"
	"k8s.io/utils/pointer"
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
	"k8s.io/apimachinery/pkg/types"
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

// Controller is the controller implementation for Foo resources
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
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue workqueue.RateLimitingInterface
	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder
}

// NewController returns a new sample controller
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

	configMapInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueConfigMap,
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldCm := oldObj.(*corev1.ConfigMap)
			newCm := newObj.(*corev1.ConfigMap)

			if reflect.DeepEqual(oldCm.Data, newCm.Data) {
				return // If the ConfigMap content does not change, the enqueue function will not be entered.
			}
			controller.enqueueConfigMap(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
			if ok {
				controller.enqueueConfigMap(tombstone.Obj)
				return
			}
			controller.enqueueConfigMap(obj)
		},
	})

	daemonSetInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueDaemonSet,
		UpdateFunc: func(oldObj, newObj interface{}) {
			controller.enqueueDaemonSet(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
			if ok {
				controller.enqueueDaemonSet(tombstone.Obj)
				return
			}
			controller.enqueueDaemonSet(obj)
		},
	})

	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: controller.handlePodUpdate,
		DeleteFunc: controller.handlePodDelete,
	})

	return controller
}

func (c *Controller) Start(ctx context.Context) error {
	return c.Run(1, ctx.Done())
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(workers int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	klog.Info("Starting DSCP client-go controller")

	// Wait for the caches to be synced before starting workers
	klog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.configMapsSynced, c.daemonSetsSynced); !ok {
		return fmt.Errorf("Failed to wait for caches to sync")
	}

	klog.Info("Starting workers")
	// Launch two workers to process Foo resources
	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	klog.Info("Started workers")
	<-stopCh
	klog.Info("Shutting down workers")

	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// Foo resource to be synced.
		if err := c.syncHandler(key); err != nil {
			// Put the item back on the workqueue to handle any transient errors.
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		} else {
			// Finally, if no error occurs we Forget this item so it does not
			// get queued again until another change happens.
			c.workqueue.Forget(obj)
		}
		klog.Infof("Successfully synced '%s'", key)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the Foo resource
// with the current status of the resource.
func (c *Controller) syncHandler(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("invalid resource key: %s", key)
	}

	// Get DSCP ConfigMap
	configMap, err := c.configMapLister.ConfigMaps(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			// DSCP ConfigMap is deleted, and DaemonSet needs to be deleted
			klog.Infof("ConfigMap %s deleted", key)
			err := c.kubeclientset.AppsV1().DaemonSets(namespace).Delete(context.TODO(), daemonSetName, metav1.DeleteOptions{})
			if err != nil {
				if errors.IsNotFound(err) {
					klog.Infof("DaemonSet %s already deleted", daemonSetName)
					return nil
				}
				return fmt.Errorf("Failed to delete DaemonSet %s: %v", daemonSetName, err)
			}

			klog.Infof("DaemonSet %s deleted due to ConfigMap %s removal", daemonSetName, key)
			return nil
		}
		return err
	}

	// Parsing DSCP ConfigMap
	var dscpConfig DscpConfig
	yamlData := configMap.Data[configMapYamlData]
	err = yaml.Unmarshal([]byte(yamlData), &dscpConfig)
	if err != nil {
		return fmt.Errorf("Unmarshal DSCP config error: %v", err)
	}

	// DSCP ConfigMap exists and is ready to create or update the corresponding DaemonSet
	timestamp := time.Now().Format(time.RFC3339)
	_, err = c.daemonSetLister.DaemonSets(namespace).Get(daemonSetName)
	if err != nil {
		if errors.IsNotFound(err) {
			// DaemonSet has not been created yet, create it
			newDS := newDaemonSetFromConfigMap(c.kubeclientset, dscpConfig, daemonSetName, timestamp)
			_, err := c.kubeclientset.AppsV1().DaemonSets(namespace).Create(context.TODO(), newDS, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("Failed to create DaemonSet %s: %v", daemonSetName, err)
			}
			klog.Infof("Created DaemonSet %s for ConfigMap %s", daemonSetName, key)
			return nil
		}
		return fmt.Errorf("Failed to get DaemonSet %s: %v", daemonSetName, err)
	}

	// DaemonSet exists, triggering a rolling update to apply the latest content of DSCP ConfigMap
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{"create-time":"%s"}}}`, timestamp))
	_, err = c.kubeclientset.AppsV1().DaemonSets(namespace).Patch(context.TODO(), daemonSetName, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("Failed to patch DaemonSet %s: %v", name, err)
	}
	klog.Infof("Patched DaemonSet %s to trigger rolling update due to ConfigMap change", name)

	// Update iptables for DSCP Pod in DaemonSet
	dscpIpMap := convertDscpIpMapFromConfigMap(c.kubeclientset, dscpConfig)
	executeCommandInPod(c.kubeclientset, generateIptablesDscpCommand(dscpIpMap, true))

	return nil
}

func convertDscpIpMapFromConfigMap(clientset kubernetes.Interface, dscpConfig DscpConfig) map[string][]string {
	dscpIpMap := make(map[string][]string)
	for _, ns := range dscpConfig.NamespaceDscpMap {
		klog.V(4).Infof("Namespace: %s, DSCP: %s", ns.Name, ns.DSCP)
		dscpIpMap[ns.DSCP] = make([]string, 0)
		pods, err := clientset.CoreV1().Pods(ns.Name).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("Error fetching Pods in namespace %s: %s", ns.Name, err.Error()))
			continue
		}

		// List the IP of each Pod
		for _, pod := range pods.Items {
			// Check if the Pod has an IP address
			if !pod.Spec.HostNetwork && pod.Status.PodIP != "" {
				dscpIpMap[ns.DSCP] = append(dscpIpMap[ns.DSCP], pod.Status.PodIP+"/32")
				klog.V(4).Infof("Pod Name: %s, IP: %s\n", pod.Name, pod.Status.PodIP)
			}
		}
	}
	return dscpIpMap
}

func generateIptablesDscpCommand(dscpIpMap map[string][]string, updateFlag bool) []string {
	commands := make([]string, 0)
	commands = append(commands, "iptables -t mangle -F")
	for dscp, podIps := range dscpIpMap {
		for _, ip := range podIps {
			markPacket := fmt.Sprintf("iptables -t mangle -A POSTROUTING -d %s -j MARK --set-mark %s", ip, dscp)
			commands = append(commands, markPacket)
		}
		setDscp := fmt.Sprintf("iptables -t mangle -A POSTROUTING -m mark --mark %s -j DSCP --set-dscp %s", dscp, dscp)
		commands = append(commands, setDscp)
	}

	// "sleep infinity" is added to prevent DaemonSet from continuously re-establishing Pods
	// because the Pod status changes to "complete" after the Pod is created and iptables commands are executed.
	if !updateFlag {
		commands = append(commands, "sleep infinity")
		fullCommandStr := strings.Join(commands, " && ")
		return []string{fullCommandStr}
	} else {
		fullCommandStr := strings.Join(commands, " && ")
		return []string{"sh", "-c", fullCommandStr}
	}
}

func executeCommandInPod(clientset kubernetes.Interface, command []string) {
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		utilruntime.HandleError(err)
		return
	}

	setSelector := labels.SelectorFromSet(labels.Set(map[string]string{"app": appName, "daemonset-owner": daemonSetName}))
	pods, err := clientset.CoreV1().Pods(configNamespace).List(
		context.TODO(),
		metav1.ListOptions{
			LabelSelector: setSelector.String(),
		},
	)

	if err != nil {
		utilruntime.HandleError(err)
		return
	}

	// All Pods under the DSCP DaemonSet need to perform exec
	for _, pod := range pods.Items {
		req := clientset.CoreV1().RESTClient().Post().
			Resource("pods").
			Name(pod.Name).
			Namespace(pod.Namespace).
			SubResource("exec")
		req.VersionedParams(&corev1.PodExecOptions{
			Command: command,
			Stderr:  true,
		}, scheme.ParameterCodec)

		// Create an executor
		exec, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("Error creating SPDY executor: %v", err))
			return
		}

		// Execute command
		var stderr bytes.Buffer
		err = exec.Stream(remotecommand.StreamOptions{
			Stderr: &stderr,
		})
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("Error executing command: %v", err))
		}
		if stderr.Len() != 0 {
			utilruntime.HandleError(fmt.Errorf(string(stderr.Bytes())))
		}
	}
}

func newDaemonSetFromConfigMap(clientset kubernetes.Interface, dscpConfig DscpConfig, daemonSetName string, timestamp string) *appsv1.DaemonSet {
	dscpIpMap := convertDscpIpMapFromConfigMap(clientset, dscpConfig)
	deployment, err := clientset.AppsV1().Deployments("default").Get(context.TODO(), "dscp-controller", metav1.GetOptions{})
	if err != nil {
		_ = fmt.Errorf("failed to get controller deployment: %v", err)
	}
	klog.Infof("Deployment: %v", deployment)
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
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "apps/v1",
					Kind:               "Deployment",
					Name:               "dscp-controller",   // name of your controller deployment
					UID:                deployment.GetUID(), // you need to pass this from your deployment
					Controller:         pointer.Bool(true),
					BlockOwnerDeletion: pointer.Bool(true),
				},
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
					Containers: []corev1.Container{
						{
							Name:            "dscp-container",
							Image:           dscpConfig.ContainerImage,
							Command:         []string{"sh", "-c"},
							Args:            generateIptablesDscpCommand(dscpIpMap, false),
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

func (c *Controller) enqueueConfigMap(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	// Unspecified ConfigMap (namespace/name) will not enter syncHandler for logical processing
	configMapKey := configNamespace + "/" + configName
	if key != configMapKey {
		klog.V(4).Infof("Non-DSCP ConfigMap: %s", key)
		return
	}
	c.workqueue.Add(key)
}

func (c *Controller) enqueueDaemonSet(obj interface{}) {
	daemonSet, ok := obj.(*appsv1.DaemonSet)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding daemon set, invalid type"))
			return
		}
		daemonSet, ok = tombstone.Obj.(*appsv1.DaemonSet)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding daemon set tombstone, invalid type"))
			return
		}
		klog.V(4).Infof("Recovered deleted daemon set '%s' from tombstone", daemonSet.GetName())
	}

	// Confirm this is a DSCP DaemonSet
	if daemonSet.Name != "k8s-dscp" || daemonSet.Namespace != configNamespace {
		return
	}

	// Check if the DSCP ConfigMap still exists
	_, err := c.configMapLister.ConfigMaps(configNamespace).Get(configName)
	if err != nil {
		if errors.IsNotFound(err) {
			klog.Infof("ConfigMap still not found, skipping recreate of DaemonSet")
			return
		}
		utilruntime.HandleError(fmt.Errorf("Failed to get ConfigMap: %v", err))
		return
	}

	// The DSCP ConfigMap still exists, add it to the workqueue.
	configMapKey := configNamespace + "/" + configName
	c.workqueue.Add(configMapKey)
}

func (c *Controller) handlePodUpdate(oldObj, newObj interface{}) {
	oldPod := oldObj.(*corev1.Pod)
	newPod := newObj.(*corev1.Pod)

	// Determine whether the Pod has changed from having no IP to having an IP
	if oldPod.Status.PodIP == "" && newPod.Status.PodIP != "" {
		execSinglePodIptables(c, newPod, false)
	}
}

func (c *Controller) handlePodDelete(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding pod, invalid type"))
			return
		}
		pod, ok = tombstone.Obj.(*corev1.Pod)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding pod tombstone, invalid type"))
			return
		}
		klog.V(4).Infof("Recovered deleted pod '%s' from tombstone", pod.GetName())
	}

	execSinglePodIptables(c, pod, true)
}

func execSinglePodIptables(c *Controller, pod *corev1.Pod, isPodDelete bool) {
	val, ok := pod.Labels["app"]
	checkAppLabel := ok && val == appName

	val, ok = pod.Labels["daemonset-owner"]
	checkOwnerLabel := ok && val == daemonSetName

	// If it is a DSCP Pod event, skip it.
	isDscpPod := checkAppLabel && checkOwnerLabel
	if isDscpPod {
		klog.V(4).Infof("Skip DSCP pod event: %s/%s", pod.Namespace, pod.Name)
		return
	}

	// Get DSCP ConfigMap
	configMap, err := c.configMapLister.ConfigMaps(configNamespace).Get(configName)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Failed to get ConfigMap: %v", err))
		return
	}

	// Parsing DSCP ConfigMap
	var dscpConfig DscpConfig
	yamlData := configMap.Data[configMapYamlData]
	err = yaml.Unmarshal([]byte(yamlData), &dscpConfig)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("execSinglePodIptables function error: %v", err))
		return
	}

	param := "-A"
	describe := "Add new pod IP in iptables"
	if isPodDelete {
		param = "-D"
		describe = "Delete pod IP in iptables"
	}

	for _, ns := range dscpConfig.NamespaceDscpMap {
		if ns.Name == pod.Namespace {
			// Check if the Pod has an IP address
			if !pod.Spec.HostNetwork && pod.Status.PodIP != "" {
				iptablesPodIp := pod.Status.PodIP + "/32"
				command := fmt.Sprintf("iptables -t mangle %s POSTROUTING -d %s -j MARK --set-mark %s", param, iptablesPodIp, ns.DSCP)
				klog.Infof("%s: %s/%s (%s)", describe, pod.Namespace, pod.Name, pod.Status.PodIP)
				executeCommandInPod(c.kubeclientset, strings.Fields(command))
				return
			}
			return
		}
	}
}
