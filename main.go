package main

import (
	"context"
	"flag"
	"path/filepath"
	"time"

	. "github.com/owenowenisme/calico-dscp-controller/dscp"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/klog"
)

func main() {
	klog.InitFlags(nil)
	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "path to the kubeconfig file")
	}
	flag.Parse()

	// Try in-cluster config first
	config, err := rest.InClusterConfig()
	if err != nil {
		// Fall back to kubeconfig
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			klog.Fatalf("Error building config: %s", err.Error())
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Error building kubernetes clientset: %s", err.Error())
	}

	// Create informer factory
	informerFactory := informers.NewSharedInformerFactory(clientset, time.Second*30)

	controller := NewDscpController(
		clientset,
		informerFactory.Core().V1().ConfigMaps(),
		informerFactory.Apps().V1().DaemonSets(),
		informerFactory.Core().V1().Pods(),
	)

	// Start informers
	informerFactory.Start(context.Background().Done())

	// Run the controller
	if err = controller.Start(context.Background()); err != nil {
		klog.Fatalf("Error running controller: %s", err.Error())
	}
}
