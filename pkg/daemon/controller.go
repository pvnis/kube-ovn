package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"time"

	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	kubeovninformer "github.com/kubeovn/kube-ovn/pkg/client/informers/externalversions"
	kubeovnlister "github.com/kubeovn/kube-ovn/pkg/client/listers/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/ovs"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

// Controller watch pod and namespace changes to update iptables, ipset and ovs qos
type Controller struct {
	config *Configuration

	providerNetworksLister          kubeovnlister.ProviderNetworkLister
	providerNetworksSynced          cache.InformerSynced
	addOrUpdateProviderNetworkQueue workqueue.RateLimitingInterface
	deleteProviderNetworkQueue      workqueue.RateLimitingInterface

	subnetsLister kubeovnlister.SubnetLister
	subnetsSynced cache.InformerSynced
	subnetQueue   workqueue.RateLimitingInterface

	podsLister listerv1.PodLister
	podsSynced cache.InformerSynced
	podQueue   workqueue.RateLimitingInterface

	nodesLister listerv1.NodeLister
	nodesSynced cache.InformerSynced

	htbQosLister kubeovnlister.HtbQosLister
	htbQosSynced cache.InformerSynced

	recorder record.EventRecorder

	protocol string

	ControllerRuntime
}

// NewController init a daemon controller
func NewController(config *Configuration, podInformerFactory informers.SharedInformerFactory, nodeInformerFactory informers.SharedInformerFactory, kubeovnInformerFactory kubeovninformer.SharedInformerFactory) (*Controller, error) {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: config.KubeClient.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: config.NodeName})

	providerNetworkInformer := kubeovnInformerFactory.Kubeovn().V1().ProviderNetworks()
	subnetInformer := kubeovnInformerFactory.Kubeovn().V1().Subnets()
	podInformer := podInformerFactory.Core().V1().Pods()
	nodeInformer := nodeInformerFactory.Core().V1().Nodes()
	htbQosInformer := kubeovnInformerFactory.Kubeovn().V1().HtbQoses()

	controller := &Controller{
		config: config,

		providerNetworksLister:          providerNetworkInformer.Lister(),
		providerNetworksSynced:          providerNetworkInformer.Informer().HasSynced,
		addOrUpdateProviderNetworkQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "AddOrUpdateProviderNetwork"),
		deleteProviderNetworkQueue:      workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "DeleteProviderNetwork"),

		subnetsLister: subnetInformer.Lister(),
		subnetsSynced: subnetInformer.Informer().HasSynced,
		subnetQueue:   workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Subnet"),

		podsLister: podInformer.Lister(),
		podsSynced: podInformer.Informer().HasSynced,
		podQueue:   workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Pod"),

		nodesLister: nodeInformer.Lister(),
		nodesSynced: nodeInformer.Informer().HasSynced,

		htbQosLister: htbQosInformer.Lister(),
		htbQosSynced: htbQosInformer.Informer().HasSynced,

		recorder: recorder,
	}

	node, err := config.KubeClient.CoreV1().Nodes().Get(context.Background(), config.NodeName, metav1.GetOptions{})
	if err != nil {
		util.LogFatalAndExit(err, "failed to get node %s info", config.NodeName)
	}
	controller.protocol = util.CheckProtocol(node.Annotations[util.IpAddressAnnotation])

	if err = controller.initRuntime(); err != nil {
		return nil, err
	}

	providerNetworkInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueAddProviderNetwork,
		UpdateFunc: controller.enqueueUpdateProviderNetwork,
		DeleteFunc: controller.enqueueDeleteProviderNetwork,
	})
	subnetInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueAddSubnet,
		UpdateFunc: controller.enqueueUpdateSubnet,
		DeleteFunc: controller.enqueueDeleteSubnet,
	})
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: controller.enqueuePod,
	})

	return controller, nil
}

func (c *Controller) enqueueAddProviderNetwork(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}

	klog.V(3).Infof("enqueue add provider network %s", key)
	c.addOrUpdateProviderNetworkQueue.Add(key)
}

func (c *Controller) enqueueUpdateProviderNetwork(old, new interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(new)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}

	klog.V(3).Infof("enqueue update provider network %s", key)
	c.addOrUpdateProviderNetworkQueue.Add(key)
}

func (c *Controller) enqueueDeleteProviderNetwork(obj interface{}) {
	klog.V(3).Infof("enqueue delete provider network %s", obj.(*kubeovnv1.ProviderNetwork).Name)
	c.deleteProviderNetworkQueue.Add(obj)
}

func (c *Controller) runAddOrUpdateProviderNetworkWorker() {
	for c.processNextAddOrUpdateProviderNetworkWorkItem() {
	}
}

func (c *Controller) runDeleteProviderNetworkWorker() {
	for c.processNextDeleteProviderNetworkWorkItem() {
	}
}

func (c *Controller) processNextAddOrUpdateProviderNetworkWorkItem() bool {
	obj, shutdown := c.addOrUpdateProviderNetworkQueue.Get()
	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer c.addOrUpdateProviderNetworkQueue.Done(obj)
		var key string
		var ok bool
		if key, ok = obj.(string); !ok {
			c.addOrUpdateProviderNetworkQueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		if err := c.handleAddOrUpdateProviderNetwork(key); err != nil {
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		c.addOrUpdateProviderNetworkQueue.Forget(obj)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		c.addOrUpdateProviderNetworkQueue.AddRateLimited(obj)
		return true
	}
	return true
}

func (c *Controller) processNextDeleteProviderNetworkWorkItem() bool {
	obj, shutdown := c.deleteProviderNetworkQueue.Get()
	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer c.deleteProviderNetworkQueue.Done(obj)
		var pn *kubeovnv1.ProviderNetwork
		var ok bool
		if pn, ok = obj.(*kubeovnv1.ProviderNetwork); !ok {
			c.deleteProviderNetworkQueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected ProviderNetwork in workqueue but got %#v", obj))
			return nil
		}
		if err := c.handleDeleteProviderNetwork(pn); err != nil {
			return fmt.Errorf("error syncing '%s': %v, requeuing", pn.Name, err)
		}
		c.deleteProviderNetworkQueue.Forget(obj)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		c.deleteProviderNetworkQueue.AddRateLimited(obj)
		return true
	}
	return true
}

func (c *Controller) handleAddOrUpdateProviderNetwork(key string) error {
	node, err := c.nodesLister.Get(c.config.NodeName)
	if err != nil {
		return err
	}
	pn, err := c.providerNetworksLister.Get(key)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	if util.ContainsString(pn.Spec.ExcludeNodes, node.Name) {
		return c.cleanProviderNetwork(pn.DeepCopy(), node.DeepCopy())
	}
	return c.initProviderNetwork(pn.DeepCopy(), node.DeepCopy())
}

func (c *Controller) initProviderNetwork(pn *kubeovnv1.ProviderNetwork, node *v1.Node) error {
	if pn.Status.EnsureNodeStandardConditions(node.Name) {
		var err error
		pn, err = c.config.KubeOvnClient.KubeovnV1().ProviderNetworks().UpdateStatus(context.Background(), pn, metav1.UpdateOptions{})
		if err != nil {
			klog.Errorf("failed to update status of provider network %s: %v", pn.Name, err)
			return err
		}
		pn = pn.DeepCopy()
	}

	nic := pn.Spec.DefaultInterface
	for _, item := range pn.Spec.CustomInterfaces {
		if util.ContainsString(item.Nodes, node.Name) {
			nic = item.Interface
			break
		}
	}

	var mtu int
	var err error
	if mtu, err = ovsInitProviderNetwork(pn.Name, nic, pn.Spec.ExchangeLinkName, c.config.MacLearningFallback); err != nil {
		if oldLen := len(node.Labels); oldLen != 0 {
			delete(node.Labels, fmt.Sprintf(util.ProviderNetworkReadyTemplate, pn.Name))
			delete(node.Labels, fmt.Sprintf(util.ProviderNetworkInterfaceTemplate, pn.Name))
			delete(node.Labels, fmt.Sprintf(util.ProviderNetworkMtuTemplate, pn.Name))
			if len(node.Labels) != oldLen {
				raw, _ := json.Marshal(node.Labels)
				patchPayload := fmt.Sprintf(`[{ "op": "replace", "path": "/metadata/labels", "value": %s }]`, raw)
				_, err1 := c.config.KubeClient.CoreV1().Nodes().Patch(context.Background(), node.Name, types.JSONPatchType, []byte(patchPayload), metav1.PatchOptions{})
				if err1 != nil {
					klog.Errorf("failed to patch node %s: %v", node.Name, err1)
				}
			}
		}

		pn.Status.SetNodeNotReady(node.Name, "InitOVSBridgeFailed", err.Error())
		if util.ContainsString(pn.Status.ReadyNodes, node.Name) {
			pn.Status.ReadyNodes = util.RemoveString(pn.Status.ReadyNodes, node.Name)
		}
		pn, err1 := c.config.KubeOvnClient.KubeovnV1().ProviderNetworks().UpdateStatus(context.Background(), pn, metav1.UpdateOptions{})
		if err1 != nil {
			klog.Errorf("failed to update status of provider network %s: %v", pn.Name, err1)
		}

		return err
	}

	pn.Status.SetNodeReady(node.Name, "InitOVSBridgeSucceeded", "")
	if !util.ContainsString(pn.Status.ReadyNodes, node.Name) {
		pn.Status.ReadyNodes = append(pn.Status.ReadyNodes, node.Name)
	}
	_, err = c.config.KubeOvnClient.KubeovnV1().ProviderNetworks().UpdateStatus(context.Background(), pn, metav1.UpdateOptions{})
	if err != nil {
		klog.Errorf("failed to update status of provider network %s: %v", pn.Name, err)
		return err
	}

	delete(node.Labels, fmt.Sprintf(util.ProviderNetworkExcludeTemplate, pn.Name))
	node.Labels[fmt.Sprintf(util.ProviderNetworkReadyTemplate, pn.Name)] = "true"
	node.Labels[fmt.Sprintf(util.ProviderNetworkInterfaceTemplate, pn.Name)] = nic
	node.Labels[fmt.Sprintf(util.ProviderNetworkMtuTemplate, pn.Name)] = strconv.Itoa(mtu)

	patchPayloadTemplate := `[{ "op": "%s", "path": "/metadata/labels", "value": %s }]`
	op := "replace"
	if len(node.Labels) == 0 {
		op = "add"
	}

	raw, _ := json.Marshal(node.Labels)
	patchPayload := fmt.Sprintf(patchPayloadTemplate, op, raw)
	_, err = c.config.KubeClient.CoreV1().Nodes().Patch(context.Background(), node.Name, types.JSONPatchType, []byte(patchPayload), metav1.PatchOptions{})
	if err != nil {
		klog.Errorf("failed to patch node %s: %v", node.Name, err)
		return err
	}

	return nil
}

func (c *Controller) updateProviderNetworkStatusForNodeDeletion(pn *kubeovnv1.ProviderNetwork, node string) error {
	var needUpdate bool
	if util.ContainsString(pn.Status.ReadyNodes, node) {
		pn.Status.ReadyNodes = util.RemoveString(pn.Status.ReadyNodes, node)
		needUpdate = true
	}
	if pn.Status.RemoveNodeConditions(node) {
		needUpdate = true
	}

	if !needUpdate {
		return nil
	}

	_, err := c.config.KubeOvnClient.KubeovnV1().ProviderNetworks().UpdateStatus(context.Background(), pn, metav1.UpdateOptions{})
	if err != nil {
		klog.Errorf("failed to update status of provider network %s: %v", pn.Name, err)
		return err
	}

	return nil
}

func (c *Controller) cleanProviderNetwork(pn *kubeovnv1.ProviderNetwork, node *v1.Node) error {
	patchPayloadTemplate := `[{ "op": "%s", "path": "/metadata/labels", "value": %s }]`
	op := "replace"
	if len(node.Labels) == 0 {
		op = "add"
	}

	var err error
	if pn.Status.RemoveNodeConditions(node.Name) {
		pn, err = c.config.KubeOvnClient.KubeovnV1().ProviderNetworks().UpdateStatus(context.Background(), pn, metav1.UpdateOptions{})
		if err != nil {
			klog.Errorf("failed to update status of provider network %s: %v", pn.Name, err)
			return err
		}
	}

	delete(node.Labels, fmt.Sprintf(util.ProviderNetworkReadyTemplate, pn.Name))
	delete(node.Labels, fmt.Sprintf(util.ProviderNetworkInterfaceTemplate, pn.Name))
	delete(node.Labels, fmt.Sprintf(util.ProviderNetworkMtuTemplate, pn.Name))
	node.Labels[fmt.Sprintf(util.ProviderNetworkExcludeTemplate, pn.Name)] = "true"
	raw, _ := json.Marshal(node.Labels)
	patchPayload := fmt.Sprintf(patchPayloadTemplate, op, raw)
	_, err = c.config.KubeClient.CoreV1().Nodes().Patch(context.Background(), node.Name, types.JSONPatchType, []byte(patchPayload), metav1.PatchOptions{})
	if err != nil {
		klog.Errorf("failed to patch node %s: %v", node.Name, err)
		return err
	}

	if err = c.updateProviderNetworkStatusForNodeDeletion(pn.DeepCopy(), node.Name); err != nil {
		return err
	}
	if err = ovsCleanProviderNetwork(pn.Name); err != nil {
		return err
	}

	return nil
}

func (c *Controller) handleDeleteProviderNetwork(pn *kubeovnv1.ProviderNetwork) error {
	if err := ovsCleanProviderNetwork(pn.Name); err != nil {
		return err
	}

	node, err := c.nodesLister.Get(c.config.NodeName)
	if err != nil {
		return err
	}
	if len(node.Labels) == 0 {
		return nil
	}

	newNode := node.DeepCopy()
	delete(newNode.Labels, fmt.Sprintf(util.ProviderNetworkReadyTemplate, pn.Name))
	delete(newNode.Labels, fmt.Sprintf(util.ProviderNetworkExcludeTemplate, pn.Name))
	delete(newNode.Labels, fmt.Sprintf(util.ProviderNetworkInterfaceTemplate, pn.Name))
	delete(newNode.Labels, fmt.Sprintf(util.ProviderNetworkMtuTemplate, pn.Name))
	raw, _ := json.Marshal(newNode.Labels)
	patchPayloadTemplate := `[{ "op": "replace", "path": "/metadata/labels", "value": %s }]`
	patchPayload := fmt.Sprintf(patchPayloadTemplate, raw)
	_, err = c.config.KubeClient.CoreV1().Nodes().Patch(context.Background(), node.Name, types.JSONPatchType, []byte(patchPayload), metav1.PatchOptions{})
	if err != nil {
		klog.Errorf("failed to patch node %s: %v", node.Name, err)
		return err
	}

	return nil
}

type subnetEvent struct {
	old, new interface{}
}

func (c *Controller) enqueueAddSubnet(obj interface{}) {
	c.subnetQueue.Add(subnetEvent{new: obj})
}

func (c *Controller) enqueueUpdateSubnet(old, new interface{}) {
	c.subnetQueue.Add(subnetEvent{old: old, new: new})
}

func (c *Controller) enqueueDeleteSubnet(obj interface{}) {
	c.subnetQueue.Add(subnetEvent{old: obj})
}

func (c *Controller) runSubnetWorker() {
	for c.processNextSubnetWorkItem() {
	}
}

func (c *Controller) processNextSubnetWorkItem() bool {
	obj, shutdown := c.subnetQueue.Get()
	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer c.subnetQueue.Done(obj)
		event, ok := obj.(subnetEvent)
		if !ok {
			c.subnetQueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected subnetEvent in workqueue but got %#v", obj))
			return nil
		}
		if err := c.reconcileRouters(event); err != nil {
			c.subnetQueue.AddRateLimited(event)
			return fmt.Errorf("error syncing '%s': %s, requeuing", event, err.Error())
		}
		c.subnetQueue.Forget(obj)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}
	return true
}

func (c *Controller) enqueuePod(old, new interface{}) {
	oldPod := old.(*v1.Pod)
	newPod := new.(*v1.Pod)

	if oldPod.Annotations[util.IngressRateAnnotation] != newPod.Annotations[util.IngressRateAnnotation] ||
		oldPod.Annotations[util.EgressRateAnnotation] != newPod.Annotations[util.EgressRateAnnotation] ||
		oldPod.Annotations[util.PriorityAnnotation] != newPod.Annotations[util.PriorityAnnotation] ||
		oldPod.Annotations[util.NetemQosLatencyAnnotation] != newPod.Annotations[util.NetemQosLatencyAnnotation] ||
		oldPod.Annotations[util.NetemQosLimitAnnotation] != newPod.Annotations[util.NetemQosLimitAnnotation] ||
		oldPod.Annotations[util.NetemQosLossAnnotation] != newPod.Annotations[util.NetemQosLossAnnotation] ||
		oldPod.Annotations[util.MirrorControlAnnotation] != newPod.Annotations[util.MirrorControlAnnotation] {
		var key string
		var err error
		if key, err = cache.MetaNamespaceKeyFunc(new); err != nil {
			utilruntime.HandleError(err)
			return
		}
		c.podQueue.Add(key)
	}

	attachNets, err := util.ParsePodNetworkAnnotation(newPod.Annotations[util.AttachmentNetworkAnnotation], newPod.Namespace)
	if err != nil {
		return
	}
	for _, multiNet := range attachNets {
		provider := fmt.Sprintf("%s.%s.ovn", multiNet.Name, multiNet.Namespace)
		if newPod.Annotations[fmt.Sprintf(util.AllocatedAnnotationTemplate, provider)] == "true" {
			if oldPod.Annotations[fmt.Sprintf(util.IngressRateAnnotationTemplate, provider)] != newPod.Annotations[fmt.Sprintf(util.IngressRateAnnotationTemplate, provider)] ||
				oldPod.Annotations[fmt.Sprintf(util.EgressRateAnnotationTemplate, provider)] != newPod.Annotations[fmt.Sprintf(util.EgressRateAnnotationTemplate, provider)] ||
				oldPod.Annotations[fmt.Sprintf(util.PriorityAnnotationTemplate, provider)] != newPod.Annotations[fmt.Sprintf(util.PriorityAnnotationTemplate, provider)] ||
				oldPod.Annotations[fmt.Sprintf(util.NetemQosLatencyAnnotationTemplate, provider)] != newPod.Annotations[fmt.Sprintf(util.NetemQosLatencyAnnotationTemplate, provider)] ||
				oldPod.Annotations[fmt.Sprintf(util.NetemQosLimitAnnotationTemplate, provider)] != newPod.Annotations[fmt.Sprintf(util.NetemQosLimitAnnotationTemplate, provider)] ||
				oldPod.Annotations[fmt.Sprintf(util.NetemQosLossAnnotationTemplate, provider)] != newPod.Annotations[fmt.Sprintf(util.NetemQosLossAnnotationTemplate, provider)] ||
				oldPod.Annotations[fmt.Sprintf(util.MirrorControlAnnotationTemplate, provider)] != newPod.Annotations[fmt.Sprintf(util.MirrorControlAnnotationTemplate, provider)] {
				var key string
				var err error
				if key, err = cache.MetaNamespaceKeyFunc(new); err != nil {
					utilruntime.HandleError(err)
					return
				}
				c.podQueue.Add(key)
			}
		}
	}
}

func (c *Controller) runPodWorker() {
	for c.processNextPodWorkItem() {
	}
}

func (c *Controller) processNextPodWorkItem() bool {
	obj, shutdown := c.podQueue.Get()

	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer c.podQueue.Done(obj)
		var key string
		var ok bool
		if key, ok = obj.(string); !ok {
			c.podQueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		if err := c.handlePod(key); err != nil {
			c.podQueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		c.podQueue.Forget(obj)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}
	return true
}

var lastNoPodOvsPort map[string]bool

func (c *Controller) markAndCleanInternalPort() error {
	klog.V(4).Infof("start to gc ovs internal ports")
	residualPorts := ovs.GetResidualInternalPorts()
	if len(residualPorts) == 0 {
		return nil
	}

	noPodOvsPort := map[string]bool{}
	for _, portName := range residualPorts {
		if !lastNoPodOvsPort[portName] {
			noPodOvsPort[portName] = true
		} else {
			klog.Infof("gc ovs internal port %s", portName)
			// Remove ovs port
			output, err := ovs.Exec(ovs.IfExists, "--with-iface", "del-port", "br-int", portName)
			if err != nil {
				return fmt.Errorf("failed to delete ovs port %v, %q", err, output)
			}
		}
	}
	lastNoPodOvsPort = noPodOvsPort

	return nil
}

func (c *Controller) deleteSubnetQos(subnet *kubeovnv1.Subnet) error {
	pods, err := c.podsLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list pods, %v", err)
		return err
	}

	for _, pod := range pods {
		if pod.Spec.HostNetwork ||
			pod.DeletionTimestamp != nil {
			continue
		}
		podName := pod.Name

		if pod.Annotations[util.LogicalSwitchAnnotation] == subnet.Name {
			// if pod's annotation for ingress-rate or priority exists, should keep qos for pod
			if pod.Annotations[util.IngressRateAnnotation] != "" ||
				pod.Annotations[util.PriorityAnnotation] != "" {
				continue
			}
			if pod.Annotations[fmt.Sprintf(util.VmTemplate, util.OvnProvider)] != "" {
				podName = pod.Annotations[fmt.Sprintf(util.VmTemplate, util.OvnProvider)]
			}
			ifaceID := ovs.PodNameToPortName(podName, pod.Namespace, util.OvnProvider)
			if err := c.clearQos(podName, pod.Namespace, ifaceID); err != nil {
				return err
			}
		}

		// clear priority for attach interface provided by kube-ovn in pod
		attachNets, err := util.ParsePodNetworkAnnotation(pod.Annotations[util.AttachmentNetworkAnnotation], pod.Namespace)
		if err != nil {
			return err
		}
		for _, multiNet := range attachNets {
			provider := fmt.Sprintf("%s.%s.ovn", multiNet.Name, multiNet.Namespace)
			if subnet.Spec.Provider != provider ||
				pod.Annotations[fmt.Sprintf(util.IngressRateAnnotationTemplate, provider)] != "" ||
				pod.Annotations[fmt.Sprintf(util.PriorityAnnotationTemplate, provider)] != "" {
				continue
			}
			if pod.Annotations[fmt.Sprintf(util.VmTemplate, provider)] != "" {
				podName = pod.Annotations[fmt.Sprintf(util.VmTemplate, provider)]
			}

			if pod.Annotations[fmt.Sprintf(util.AllocatedAnnotationTemplate, provider)] == "true" {
				ifaceID := ovs.PodNameToPortName(podName, pod.Namespace, provider)
				if err := c.clearQos(podName, pod.Namespace, ifaceID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (c *Controller) getSubnetQosPriority(subnetName string) string {
	var priority string
	subnet, err := c.subnetsLister.Get(subnetName)
	if err != nil {
		klog.Errorf("failed to get subnet %s: %v", subnet, err)
	} else if subnet.Spec.HtbQos != "" {
		htbQos, err := c.htbQosLister.Get(subnet.Spec.HtbQos)
		if err != nil {
			klog.Errorf("failed to get htbqos %s: %v", subnet.Spec.HtbQos, err)
		} else {
			priority = htbQos.Spec.Priority
		}
	}
	return priority
}

// Run starts controller
func (c *Controller) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.addOrUpdateProviderNetworkQueue.ShutDown()
	defer c.deleteProviderNetworkQueue.ShutDown()
	defer c.subnetQueue.ShutDown()
	defer c.podQueue.ShutDown()

	go wait.Until(ovs.CleanLostInterface, time.Minute, stopCh)
	go wait.Until(recompute, 10*time.Minute, stopCh)
	go wait.Until(rotateLog, 1*time.Hour, stopCh)
	go wait.Until(c.operateMod, 10*time.Second, stopCh)

	if ok := cache.WaitForCacheSync(stopCh, c.providerNetworksSynced, c.subnetsSynced, c.podsSynced, c.nodesSynced, c.htbQosSynced); !ok {
		util.LogFatalAndExit(nil, "failed to wait for caches to sync")
	}

	if err := c.setIPSet(); err != nil {
		util.LogFatalAndExit(err, "failed to set ipsets")
	}

	klog.Info("Started workers")
	go wait.Until(c.loopOvn0Check, 5*time.Second, stopCh)
	go wait.Until(c.runAddOrUpdateProviderNetworkWorker, time.Second, stopCh)
	go wait.Until(c.runDeleteProviderNetworkWorker, time.Second, stopCh)
	go wait.Until(c.runSubnetWorker, time.Second, stopCh)
	go wait.Until(c.runPodWorker, time.Second, stopCh)
	go wait.Until(c.runGateway, 3*time.Second, stopCh)
	go wait.Until(c.loopEncapIpCheck, 3*time.Second, stopCh)
	go wait.Until(func() {
		if err := c.markAndCleanInternalPort(); err != nil {
			klog.Errorf("gc ovs port error: %v", err)
		}
	}, 5*time.Minute, stopCh)

	<-stopCh
	klog.Info("Shutting down workers")
}

func recompute() {
	output, err := exec.Command("ovn-appctl", "-t", "ovn-controller", "inc-engine/recompute").CombinedOutput()
	if err != nil {
		klog.Errorf("failed to recompute ovn-controller %q", output)
	}
}
