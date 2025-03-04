package controller

import (
	"sync"
	"time"

	"github.com/neverlee/keymutex"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	v1 "k8s.io/client-go/listers/core/v1"
	netv1 "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	kubeovninformer "github.com/kubeovn/kube-ovn/pkg/client/informers/externalversions"
	kubeovnlister "github.com/kubeovn/kube-ovn/pkg/client/listers/kubeovn/v1"
	ovnipam "github.com/kubeovn/kube-ovn/pkg/ipam"
	"github.com/kubeovn/kube-ovn/pkg/ovs"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

const controllerAgentName = "kube-ovn-controller"

// Controller is kube-ovn main controller that watch ns/pod/node/svc/ep and operate ovn
type Controller struct {
	config *Configuration
	vpcs   *sync.Map
	//subnetVpcMap *sync.Map
	podSubnetMap *sync.Map
	ipam         *ovnipam.IPAM

	ovnLegacyClient *ovs.LegacyClient
	ovnClient       *ovs.OvnClient
	ovnPgKeyMutex   *keymutex.KeyMutex

	podsLister             v1.PodLister
	podsSynced             cache.InformerSynced
	addPodQueue            workqueue.RateLimitingInterface
	deletePodQueue         workqueue.RateLimitingInterface
	updatePodQueue         workqueue.RateLimitingInterface
	updatePodSecurityQueue workqueue.RateLimitingInterface
	podKeyMutex            *keymutex.KeyMutex

	vpcsLister           kubeovnlister.VpcLister
	vpcSynced            cache.InformerSynced
	addOrUpdateVpcQueue  workqueue.RateLimitingInterface
	delVpcQueue          workqueue.RateLimitingInterface
	updateVpcStatusQueue workqueue.RateLimitingInterface

	vpcNatGatewayLister           kubeovnlister.VpcNatGatewayLister
	vpcNatGatewaySynced           cache.InformerSynced
	addOrUpdateVpcNatGatewayQueue workqueue.RateLimitingInterface
	delVpcNatGatewayQueue         workqueue.RateLimitingInterface
	initVpcNatGatewayQueue        workqueue.RateLimitingInterface
	updateVpcEipQueue             workqueue.RateLimitingInterface
	updateVpcFloatingIpQueue      workqueue.RateLimitingInterface
	updateVpcDnatQueue            workqueue.RateLimitingInterface
	updateVpcSnatQueue            workqueue.RateLimitingInterface
	updateVpcSubnetQueue          workqueue.RateLimitingInterface
	vpcNatGwKeyMutex              *keymutex.KeyMutex

	switchLBRuleLister      kubeovnlister.SwitchLBRuleLister
	switchLBRuleSynced      cache.InformerSynced
	addSwitchLBRuleQueue    workqueue.RateLimitingInterface
	UpdateSwitchLBRuleQueue workqueue.RateLimitingInterface
	delSwitchLBRuleQueue    workqueue.RateLimitingInterface

	vpcDnsLister           kubeovnlister.VpcDnsLister
	vpcDnsSynced           cache.InformerSynced
	addOrUpdateVpcDnsQueue workqueue.RateLimitingInterface
	delVpcDnsQueue         workqueue.RateLimitingInterface

	subnetsLister           kubeovnlister.SubnetLister
	subnetSynced            cache.InformerSynced
	addOrUpdateSubnetQueue  workqueue.RateLimitingInterface
	deleteSubnetQueue       workqueue.RateLimitingInterface
	deleteRouteQueue        workqueue.RateLimitingInterface
	updateSubnetStatusQueue workqueue.RateLimitingInterface
	syncVirtualPortsQueue   workqueue.RateLimitingInterface
	subnetStatusKeyMutex    *keymutex.KeyMutex

	ipsLister kubeovnlister.IPLister
	ipSynced  cache.InformerSynced

	virtualIpsLister     kubeovnlister.VipLister
	virtualIpsSynced     cache.InformerSynced
	addVirtualIpQueue    workqueue.RateLimitingInterface
	updateVirtualIpQueue workqueue.RateLimitingInterface
	delVirtualIpQueue    workqueue.RateLimitingInterface

	iptablesEipsLister     kubeovnlister.IptablesEIPLister
	iptablesEipSynced      cache.InformerSynced
	addIptablesEipQueue    workqueue.RateLimitingInterface
	updateIptablesEipQueue workqueue.RateLimitingInterface
	resetIptablesEipQueue  workqueue.RateLimitingInterface
	delIptablesEipQueue    workqueue.RateLimitingInterface

	podAnnotatedIptablesEipLister      v1.PodLister
	podAnnotatedIptablesEipSynced      cache.InformerSynced
	addPodAnnotatedIptablesEipQueue    workqueue.RateLimitingInterface
	updatePodAnnotatedIptablesEipQueue workqueue.RateLimitingInterface
	delPodAnnotatedIptablesEipQueue    workqueue.RateLimitingInterface

	iptablesFipsLister     kubeovnlister.IptablesFIPRuleLister
	iptablesFipSynced      cache.InformerSynced
	addIptablesFipQueue    workqueue.RateLimitingInterface
	updateIptablesFipQueue workqueue.RateLimitingInterface
	delIptablesFipQueue    workqueue.RateLimitingInterface

	podAnnotatedIptablesFipLister      v1.PodLister
	podAnnotatedIptablesFipSynced      cache.InformerSynced
	addPodAnnotatedIptablesFipQueue    workqueue.RateLimitingInterface
	updatePodAnnotatedIptablesFipQueue workqueue.RateLimitingInterface
	delPodAnnotatedIptablesFipQueue    workqueue.RateLimitingInterface

	iptablesDnatRulesLister     kubeovnlister.IptablesDnatRuleLister
	iptablesDnatRuleSynced      cache.InformerSynced
	addIptablesDnatRuleQueue    workqueue.RateLimitingInterface
	updateIptablesDnatRuleQueue workqueue.RateLimitingInterface
	delIptablesDnatRuleQueue    workqueue.RateLimitingInterface

	iptablesSnatRulesLister     kubeovnlister.IptablesSnatRuleLister
	iptablesSnatRuleSynced      cache.InformerSynced
	addIptablesSnatRuleQueue    workqueue.RateLimitingInterface
	updateIptablesSnatRuleQueue workqueue.RateLimitingInterface
	delIptablesSnatRuleQueue    workqueue.RateLimitingInterface

	ovnEipsLister     kubeovnlister.OvnEipLister
	ovnEipSynced      cache.InformerSynced
	addOvnEipQueue    workqueue.RateLimitingInterface
	updateOvnEipQueue workqueue.RateLimitingInterface
	resetOvnEipQueue  workqueue.RateLimitingInterface
	delOvnEipQueue    workqueue.RateLimitingInterface

	ovnFipsLister     kubeovnlister.OvnFipLister
	ovnFipSynced      cache.InformerSynced
	addOvnFipQueue    workqueue.RateLimitingInterface
	updateOvnFipQueue workqueue.RateLimitingInterface
	delOvnFipQueue    workqueue.RateLimitingInterface

	ovnSnatRulesLister     kubeovnlister.OvnSnatRuleLister
	ovnSnatRuleSynced      cache.InformerSynced
	addOvnSnatRuleQueue    workqueue.RateLimitingInterface
	updateOvnSnatRuleQueue workqueue.RateLimitingInterface
	delOvnSnatRuleQueue    workqueue.RateLimitingInterface

	vlansLister kubeovnlister.VlanLister
	vlanSynced  cache.InformerSynced

	providerNetworksLister     kubeovnlister.ProviderNetworkLister
	providerNetworkSynced      cache.InformerSynced
	updateProviderNetworkQueue workqueue.RateLimitingInterface

	addVlanQueue    workqueue.RateLimitingInterface
	delVlanQueue    workqueue.RateLimitingInterface
	updateVlanQueue workqueue.RateLimitingInterface

	namespacesLister  v1.NamespaceLister
	namespacesSynced  cache.InformerSynced
	addNamespaceQueue workqueue.RateLimitingInterface

	nodesLister     v1.NodeLister
	nodesSynced     cache.InformerSynced
	addNodeQueue    workqueue.RateLimitingInterface
	updateNodeQueue workqueue.RateLimitingInterface
	deleteNodeQueue workqueue.RateLimitingInterface

	servicesLister     v1.ServiceLister
	serviceSynced      cache.InformerSynced
	addServiceQueue    workqueue.RateLimitingInterface
	deleteServiceQueue workqueue.RateLimitingInterface
	updateServiceQueue workqueue.RateLimitingInterface

	endpointsLister     v1.EndpointsLister
	endpointsSynced     cache.InformerSynced
	updateEndpointQueue workqueue.RateLimitingInterface

	npsLister     netv1.NetworkPolicyLister
	npsSynced     cache.InformerSynced
	updateNpQueue workqueue.RateLimitingInterface
	deleteNpQueue workqueue.RateLimitingInterface

	sgsLister          kubeovnlister.SecurityGroupLister
	sgSynced           cache.InformerSynced
	addOrUpdateSgQueue workqueue.RateLimitingInterface
	delSgQueue         workqueue.RateLimitingInterface
	syncSgPortsQueue   workqueue.RateLimitingInterface
	sgKeyMutex         *keymutex.KeyMutex

	configMapsLister v1.ConfigMapLister
	configMapsSynced cache.InformerSynced

	recorder               record.EventRecorder
	informerFactory        kubeinformers.SharedInformerFactory
	cmInformerFactory      kubeinformers.SharedInformerFactory
	kubeovnInformerFactory kubeovninformer.SharedInformerFactory
	elector                *leaderelection.LeaderElector
}

// NewController returns a new ovn controller
func NewController(config *Configuration) *Controller {
	utilruntime.Must(kubeovnv1.AddToScheme(scheme.Scheme))
	klog.V(4).Info("Creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: config.KubeFactoryClient.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})
	custCrdRateLimiter := workqueue.NewMaxOfRateLimiter(
		workqueue.NewItemExponentialFailureRateLimiter(time.Duration(config.CustCrdRetryMinDelay)*time.Second, time.Duration(config.CustCrdRetryMaxDelay)*time.Second),
		&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
	)

	informerFactory := kubeinformers.NewSharedInformerFactoryWithOptions(config.KubeFactoryClient, 0,
		kubeinformers.WithTweakListOptions(func(listOption *metav1.ListOptions) {
			listOption.AllowWatchBookmarks = true
		}))
	cmInformerFactory := kubeinformers.NewSharedInformerFactoryWithOptions(config.KubeFactoryClient, 0,
		kubeinformers.WithTweakListOptions(func(listOption *metav1.ListOptions) {
			listOption.AllowWatchBookmarks = true
		}), kubeinformers.WithNamespace(config.PodNamespace))
	kubeovnInformerFactory := kubeovninformer.NewSharedInformerFactoryWithOptions(config.KubeOvnFactoryClient, 0,
		kubeovninformer.WithTweakListOptions(func(listOption *metav1.ListOptions) {
			listOption.AllowWatchBookmarks = true
		}))

	vpcInformer := kubeovnInformerFactory.Kubeovn().V1().Vpcs()
	vpcNatGatewayInformer := kubeovnInformerFactory.Kubeovn().V1().VpcNatGateways()
	subnetInformer := kubeovnInformerFactory.Kubeovn().V1().Subnets()
	ipInformer := kubeovnInformerFactory.Kubeovn().V1().IPs()
	virtualIpInformer := kubeovnInformerFactory.Kubeovn().V1().Vips()
	iptablesEipInformer := kubeovnInformerFactory.Kubeovn().V1().IptablesEIPs()
	iptablesFipInformer := kubeovnInformerFactory.Kubeovn().V1().IptablesFIPRules()
	iptablesDnatRuleInformer := kubeovnInformerFactory.Kubeovn().V1().IptablesDnatRules()
	iptablesSnatRuleInformer := kubeovnInformerFactory.Kubeovn().V1().IptablesSnatRules()
	vlanInformer := kubeovnInformerFactory.Kubeovn().V1().Vlans()
	providerNetworkInformer := kubeovnInformerFactory.Kubeovn().V1().ProviderNetworks()
	sgInformer := kubeovnInformerFactory.Kubeovn().V1().SecurityGroups()
	podInformer := informerFactory.Core().V1().Pods()
	podAnnotatedIptablesEipInformer := informerFactory.Core().V1().Pods()
	podAnnotatedIptablesFipInformer := informerFactory.Core().V1().Pods()
	namespaceInformer := informerFactory.Core().V1().Namespaces()
	nodeInformer := informerFactory.Core().V1().Nodes()
	serviceInformer := informerFactory.Core().V1().Services()
	endpointInformer := informerFactory.Core().V1().Endpoints()
	configMapInformer := cmInformerFactory.Core().V1().ConfigMaps()

	controller := &Controller{
		config:          config,
		vpcs:            &sync.Map{},
		podSubnetMap:    &sync.Map{},
		ovnLegacyClient: ovs.NewLegacyClient(config.OvnNbAddr, config.OvnTimeout, config.OvnSbAddr, config.ClusterRouter, config.ClusterTcpLoadBalancer, config.ClusterUdpLoadBalancer, config.ClusterTcpSessionLoadBalancer, config.ClusterUdpSessionLoadBalancer, config.NodeSwitch, config.NodeSwitchCIDR),
		ovnPgKeyMutex:   keymutex.New(97),
		ipam:            ovnipam.NewIPAM(),

		vpcsLister:           vpcInformer.Lister(),
		vpcSynced:            vpcInformer.Informer().HasSynced,
		addOrUpdateVpcQueue:  workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "AddOrUpdateVpc"),
		delVpcQueue:          workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "DeleteVpc"),
		updateVpcStatusQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "UpdateVpcStatus"),

		vpcNatGatewayLister:           vpcNatGatewayInformer.Lister(),
		vpcNatGatewaySynced:           vpcNatGatewayInformer.Informer().HasSynced,
		addOrUpdateVpcNatGatewayQueue: workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "AddOrUpdateVpcNatGw"),
		initVpcNatGatewayQueue:        workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "InitVpcNatGw"),
		delVpcNatGatewayQueue:         workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "DeleteVpcNatGw"),
		updateVpcEipQueue:             workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "UpdateVpcEip"),
		updateVpcFloatingIpQueue:      workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "UpdateVpcFloatingIp"),
		updateVpcDnatQueue:            workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "UpdateVpcDnat"),
		updateVpcSnatQueue:            workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "UpdateVpcSnat"),
		updateVpcSubnetQueue:          workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "UpdateVpcSubnet"),
		vpcNatGwKeyMutex:              keymutex.New(97),

		subnetsLister:           subnetInformer.Lister(),
		subnetSynced:            subnetInformer.Informer().HasSynced,
		addOrUpdateSubnetQueue:  workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "AddSubnet"),
		deleteSubnetQueue:       workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "DeleteSubnet"),
		deleteRouteQueue:        workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "DeleteRoute"),
		updateSubnetStatusQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "UpdateSubnetStatus"),
		syncVirtualPortsQueue:   workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "SyncVirtualPort"),
		subnetStatusKeyMutex:    keymutex.New(97),

		ipsLister: ipInformer.Lister(),
		ipSynced:  ipInformer.Informer().HasSynced,

		virtualIpsLister:     virtualIpInformer.Lister(),
		virtualIpsSynced:     virtualIpInformer.Informer().HasSynced,
		addVirtualIpQueue:    workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "addVirtualIp"),
		updateVirtualIpQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "updateVirtualIp"),
		delVirtualIpQueue:    workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "delVirtualIp"),

		iptablesEipsLister:     iptablesEipInformer.Lister(),
		iptablesEipSynced:      iptablesEipInformer.Informer().HasSynced,
		addIptablesEipQueue:    workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "addIptablesEip"),
		updateIptablesEipQueue: workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "updateIptablesEip"),
		resetIptablesEipQueue:  workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "resetIptablesEip"),
		delIptablesEipQueue:    workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "delIptablesEip"),

		podAnnotatedIptablesEipLister:      podAnnotatedIptablesEipInformer.Lister(),
		podAnnotatedIptablesEipSynced:      podAnnotatedIptablesEipInformer.Informer().HasSynced,
		addPodAnnotatedIptablesEipQueue:    workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "addPodAnnotatedIptablesEip"),
		updatePodAnnotatedIptablesEipQueue: workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "updatePodAnnotatedIptablesEip"),
		delPodAnnotatedIptablesEipQueue:    workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "delPodAnnotatedIptablesEip"),

		iptablesFipsLister:     iptablesFipInformer.Lister(),
		iptablesFipSynced:      iptablesFipInformer.Informer().HasSynced,
		addIptablesFipQueue:    workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "addIptablesFip"),
		updateIptablesFipQueue: workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "updateIptablesFip"),
		delIptablesFipQueue:    workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "delIptablesFip"),

		podAnnotatedIptablesFipLister:      podAnnotatedIptablesFipInformer.Lister(),
		podAnnotatedIptablesFipSynced:      podAnnotatedIptablesFipInformer.Informer().HasSynced,
		addPodAnnotatedIptablesFipQueue:    workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "addPodAnnotatedIptablesFip"),
		updatePodAnnotatedIptablesFipQueue: workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "updatePodAnnotatedIptablesFip"),
		delPodAnnotatedIptablesFipQueue:    workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "delPodAnnotatedIptablesFip"),

		iptablesDnatRulesLister:     iptablesDnatRuleInformer.Lister(),
		iptablesDnatRuleSynced:      iptablesDnatRuleInformer.Informer().HasSynced,
		addIptablesDnatRuleQueue:    workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "addIptablesDnatRule"),
		updateIptablesDnatRuleQueue: workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "updateIptablesDnatRule"),
		delIptablesDnatRuleQueue:    workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "delIptablesDnatRule"),

		iptablesSnatRulesLister:     iptablesSnatRuleInformer.Lister(),
		iptablesSnatRuleSynced:      iptablesSnatRuleInformer.Informer().HasSynced,
		addIptablesSnatRuleQueue:    workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "addIptablesSnatRule"),
		updateIptablesSnatRuleQueue: workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "updateIptablesSnatRule"),
		delIptablesSnatRuleQueue:    workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "delIptablesSnatRule"),

		vlansLister:     vlanInformer.Lister(),
		vlanSynced:      vlanInformer.Informer().HasSynced,
		addVlanQueue:    workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "AddVlan"),
		delVlanQueue:    workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "DelVlan"),
		updateVlanQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "UpdateVlan"),

		providerNetworksLister:     providerNetworkInformer.Lister(),
		providerNetworkSynced:      providerNetworkInformer.Informer().HasSynced,
		updateProviderNetworkQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "UpdateProviderNetwork"),

		podsLister:             podInformer.Lister(),
		podsSynced:             podInformer.Informer().HasSynced,
		addPodQueue:            workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "AddPod"),
		deletePodQueue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "DeletePod"),
		updatePodQueue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "UpdatePod"),
		updatePodSecurityQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "UpdatePodSecurity"),
		podKeyMutex:            keymutex.New(97),

		namespacesLister:  namespaceInformer.Lister(),
		namespacesSynced:  namespaceInformer.Informer().HasSynced,
		addNamespaceQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "AddNamespace"),

		nodesLister:     nodeInformer.Lister(),
		nodesSynced:     nodeInformer.Informer().HasSynced,
		addNodeQueue:    workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "AddNode"),
		updateNodeQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "UpdateNode"),
		deleteNodeQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "DeleteNode"),

		servicesLister:     serviceInformer.Lister(),
		serviceSynced:      serviceInformer.Informer().HasSynced,
		addServiceQueue:    workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "AddService"),
		deleteServiceQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "DeleteService"),
		updateServiceQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "UpdateService"),

		endpointsLister:     endpointInformer.Lister(),
		endpointsSynced:     endpointInformer.Informer().HasSynced,
		updateEndpointQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "UpdateEndpoint"),

		configMapsLister: configMapInformer.Lister(),
		configMapsSynced: configMapInformer.Informer().HasSynced,

		recorder: recorder,

		sgsLister:          sgInformer.Lister(),
		sgSynced:           sgInformer.Informer().HasSynced,
		addOrUpdateSgQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "UpdateSg"),
		delSgQueue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "DeleteSg"),
		syncSgPortsQueue:   workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "SyncSgPorts"),
		sgKeyMutex:         keymutex.New(97),

		informerFactory:        informerFactory,
		cmInformerFactory:      cmInformerFactory,
		kubeovnInformerFactory: kubeovnInformerFactory,
	}

	var err error
	if controller.ovnClient, err = ovs.NewOvnClient(config.OvnNbAddr, config.OvnTimeout); err != nil {
		klog.Fatal(err)
	}

	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueAddPod,
		DeleteFunc: controller.enqueueDeletePod,
		UpdateFunc: controller.enqueueUpdatePod,
	})

	namespaceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueAddNamespace,
		UpdateFunc: controller.enqueueUpdateNamespace,
		DeleteFunc: controller.enqueueDeleteNamespace,
	})

	nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueAddNode,
		UpdateFunc: controller.enqueueUpdateNode,
		DeleteFunc: controller.enqueueDeleteNode,
	})

	serviceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueAddService,
		DeleteFunc: controller.enqueueDeleteService,
		UpdateFunc: controller.enqueueUpdateService,
	})

	endpointInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueAddEndpoint,
		UpdateFunc: controller.enqueueUpdateEndpoint,
	})

	vpcInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueAddVpc,
		UpdateFunc: controller.enqueueUpdateVpc,
		DeleteFunc: controller.enqueueDelVpc,
	})

	vpcNatGatewayInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueAddVpcNatGw,
		UpdateFunc: controller.enqueueUpdateVpcNatGw,
		DeleteFunc: controller.enqueueDeleteVpcNatGw,
	})

	if config.EnableLb {
		switchLBRuleInformer := kubeovnInformerFactory.Kubeovn().V1().SwitchLBRules()
		controller.switchLBRuleLister = switchLBRuleInformer.Lister()
		controller.switchLBRuleSynced = switchLBRuleInformer.Informer().HasSynced
		controller.addSwitchLBRuleQueue = workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "addSwitchLBRule")
		controller.delSwitchLBRuleQueue = workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "delSwitchLBRule")
		controller.UpdateSwitchLBRuleQueue = workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "updateSwitchLBRule")

		switchLBRuleInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    controller.enqueueAddSwitchLBRule,
			UpdateFunc: controller.enqueueUpdateSwitchLBRule,
			DeleteFunc: controller.enqueueDeleteSwitchLBRule,
		})

		vpcDnsInformer := kubeovnInformerFactory.Kubeovn().V1().VpcDnses()
		controller.vpcDnsLister = vpcDnsInformer.Lister()
		controller.vpcDnsSynced = vpcDnsInformer.Informer().HasSynced
		controller.addOrUpdateVpcDnsQueue = workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "AddOrUpdateVpcDns")
		controller.delVpcDnsQueue = workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "DeleteVpcDns")
		vpcDnsInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    controller.enqueueAddVpcDns,
			UpdateFunc: controller.enqueueUpdateVpcDns,
			DeleteFunc: controller.enqueueDeleteVpcDns,
		})
	}

	subnetInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueAddSubnet,
		UpdateFunc: controller.enqueueUpdateSubnet,
		DeleteFunc: controller.enqueueDeleteSubnet,
	})

	ipInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueAddOrDelIP,
		UpdateFunc: controller.enqueueUpdateIP,
		DeleteFunc: controller.enqueueAddOrDelIP,
	})

	vlanInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueAddVlan,
		DeleteFunc: controller.enqueueDelVlan,
		UpdateFunc: controller.enqueueUpdateVlan,
	})

	providerNetworkInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: controller.enqueueUpdateProviderNetwork,
	})

	if config.EnableNP {
		npInformer := informerFactory.Networking().V1().NetworkPolicies()
		controller.npsLister = npInformer.Lister()
		controller.npsSynced = npInformer.Informer().HasSynced
		controller.updateNpQueue = workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "UpdateNp")
		controller.deleteNpQueue = workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "DeleteNp")
		npInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    controller.enqueueAddNp,
			UpdateFunc: controller.enqueueUpdateNp,
			DeleteFunc: controller.enqueueDeleteNp,
		})
	}
	sgInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueAddSg,
		DeleteFunc: controller.enqueueDeleteSg,
		UpdateFunc: controller.enqueueUpdateSg,
	})

	virtualIpInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueAddVirtualIp,
		UpdateFunc: controller.enqueueUpdateVirtualIp,
		DeleteFunc: controller.enqueueDelVirtualIp,
	})

	iptablesEipInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueAddIptablesEip,
		UpdateFunc: controller.enqueueUpdateIptablesEip,
		DeleteFunc: controller.enqueueDelIptablesEip,
	})

	iptablesFipInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueAddIptablesFip,
		UpdateFunc: controller.enqueueUpdateIptablesFip,
		DeleteFunc: controller.enqueueDelIptablesFip,
	})

	iptablesDnatRuleInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueAddIptablesDnatRule,
		UpdateFunc: controller.enqueueUpdateIptablesDnatRule,
		DeleteFunc: controller.enqueueDelIptablesDnatRule,
	})

	iptablesSnatRuleInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueAddIptablesSnatRule,
		UpdateFunc: controller.enqueueUpdateIptablesSnatRule,
		DeleteFunc: controller.enqueueDelIptablesSnatRule,
	})
	if config.EnableEipSnat {
		ovnEipInformer := kubeovnInformerFactory.Kubeovn().V1().OvnEips()
		controller.ovnEipsLister = ovnEipInformer.Lister()
		controller.ovnEipSynced = ovnEipInformer.Informer().HasSynced
		controller.addOvnEipQueue = workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "addOvnEip")
		controller.updateOvnEipQueue = workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "updateOvnEip")
		controller.resetOvnEipQueue = workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "resetOvnEip")
		controller.delOvnEipQueue = workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "delOvnEip")

		ovnEipInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    controller.enqueueAddOvnEip,
			UpdateFunc: controller.enqueueUpdateOvnEip,
			DeleteFunc: controller.enqueueDelOvnEip,
		})

		ovnFipInformer := kubeovnInformerFactory.Kubeovn().V1().OvnFips()
		controller.ovnFipsLister = ovnFipInformer.Lister()
		controller.ovnFipSynced = ovnFipInformer.Informer().HasSynced
		controller.addOvnFipQueue = workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "addOvnFip")
		controller.updateOvnFipQueue = workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "updateOvnFip")
		controller.delOvnFipQueue = workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "delOvnFip")
		ovnFipInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    controller.enqueueAddOvnFip,
			UpdateFunc: controller.enqueueUpdateOvnFip,
			DeleteFunc: controller.enqueueDelOvnFip,
		})

		ovnSnatRuleInformer := kubeovnInformerFactory.Kubeovn().V1().OvnSnatRules()
		controller.ovnSnatRulesLister = ovnSnatRuleInformer.Lister()
		controller.ovnSnatRuleSynced = ovnSnatRuleInformer.Informer().HasSynced
		controller.addOvnSnatRuleQueue = workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "addOvnSnatRule")
		controller.updateOvnSnatRuleQueue = workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "updateOvnSnatRule")
		controller.delOvnSnatRuleQueue = workqueue.NewNamedRateLimitingQueue(custCrdRateLimiter, "delOvnSnatRule")
		ovnSnatRuleInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    controller.enqueueAddOvnSnatRule,
			UpdateFunc: controller.enqueueUpdateOvnSnatRule,
			DeleteFunc: controller.enqueueDelOvnSnatRule,
		})
	}

	podAnnotatedIptablesEipInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueAddPodAnnotatedIptablesEip,
		UpdateFunc: controller.enqueueUpdatePodAnnotatedIptablesEip,
		DeleteFunc: controller.enqueueDeletePodAnnotatedIptablesEip,
	})
	podAnnotatedIptablesFipInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueueAddPodAnnotatedIptablesFip,
		UpdateFunc: controller.enqueueUpdatePodAnnotatedIptablesFip,
		DeleteFunc: controller.enqueueDeletePodAnnotatedIptablesFip,
	})
	return controller
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(stopCh <-chan struct{}) {
	defer c.shutdown()
	klog.Info("Starting OVN controller")

	// wait for becoming a leader
	c.leaderElection()

	// Wait for the caches to be synced before starting workers
	c.informerFactory.Start(stopCh)
	c.cmInformerFactory.Start(stopCh)
	c.kubeovnInformerFactory.Start(stopCh)

	klog.Info("Waiting for informer caches to sync")
	cacheSyncs := []cache.InformerSynced{
		c.vpcNatGatewaySynced, c.vpcSynced, c.subnetSynced,
		c.ipSynced, c.virtualIpsSynced, c.iptablesEipSynced,
		c.iptablesFipSynced, c.iptablesDnatRuleSynced, c.iptablesSnatRuleSynced,
		c.podAnnotatedIptablesEipSynced, c.podAnnotatedIptablesFipSynced,
		c.vlanSynced, c.podsSynced, c.namespacesSynced, c.nodesSynced,
		c.serviceSynced, c.endpointsSynced, c.configMapsSynced,
	}

	if c.config.EnableEipSnat {
		cacheSyncs = append(cacheSyncs, c.ovnEipSynced, c.ovnFipSynced, c.ovnSnatRuleSynced)
	}

	if c.config.EnableNP {
		cacheSyncs = append(cacheSyncs, c.npsSynced)
	}

	if c.config.EnableLb {
		cacheSyncs = append(cacheSyncs, c.switchLBRuleSynced, c.vpcDnsSynced)
	}

	if ok := cache.WaitForCacheSync(stopCh, cacheSyncs...); !ok {
		util.LogFatalAndExit(nil, "failed to wait for caches to sync")
	}

	if err := c.ovnLegacyClient.SetLsDnatModDlDst(c.config.LsDnatModDlDst); err != nil {
		util.LogFatalAndExit(err, "failed to set NB_Global option ls_dnat_mod_dl_dst")
	}
	if err := c.ovnLegacyClient.SetUseCtInvMatch(); err != nil {
		util.LogFatalAndExit(err, "failed to set NB_Global option use_ct_inv_match")
	}

	if err := c.InitDefaultVpc(); err != nil {
		util.LogFatalAndExit(err, "failed to initialize default vpc")
	}

	if err := c.InitOVN(); err != nil {
		util.LogFatalAndExit(err, "failed to initialize ovn resources")
	}

	// sync ip crd before initIPAM since ip crd will be used to restore vm and statefulset pod in initIPAM
	if err := c.initSyncCrdIPs(); err != nil {
		util.LogFatalAndExit(err, "failed to sync crd ips")
	}

	if err := c.InitIPAM(); err != nil {
		util.LogFatalAndExit(err, "failed to initialize ipam")
	}

	if err := c.initNodeChassis(); err != nil {
		util.LogFatalAndExit(err, "failed to initialize node chassis")
	}

	if err := c.initNodeRoutes(); err != nil {
		util.LogFatalAndExit(err, "failed to initialize node routes")
	}

	if err := c.initDenyAllSecurityGroup(); err != nil {
		util.LogFatalAndExit(err, "failed to initialize 'deny_all' security group")
	}

	// remove resources in ovndb that not exist any more in kubernetes resources
	if err := c.gc(); err != nil {
		util.LogFatalAndExit(err, "failed to run gc")
	}

	c.registerSubnetMetrics()
	if err := c.initSyncCrdSubnets(); err != nil {
		util.LogFatalAndExit(err, "failed to sync crd subnets")
	}
	if err := c.initSyncCrdVlans(); err != nil {
		util.LogFatalAndExit(err, "failed to sync crd vlans")
	}

	if c.config.PodDefaultFipType == util.IptablesFip {
		if err := c.initSyncCrdVpcNatGw(); err != nil {
			util.LogFatalAndExit(err, "failed to sync crd vpc nat gateways")
		}
	}

	if c.config.EnableLb {
		if err := c.initVpcDnsConfig(); err != nil {
			util.LogFatalAndExit(err, "failed to initialize vpc-dns")
		}
	}

	if err := c.addNodeGwStaticRoute(); err != nil {
		util.LogFatalAndExit(err, "failed to add static route for node gateway")
	}

	// start workers to do all the network operations
	c.startWorkers(stopCh)
	<-stopCh
	klog.Info("Shutting down workers")
}

func (c *Controller) shutdown() {
	utilruntime.HandleCrash()

	c.addPodQueue.ShutDown()
	c.deletePodQueue.ShutDown()
	c.updatePodQueue.ShutDown()
	c.updatePodSecurityQueue.ShutDown()

	c.addNamespaceQueue.ShutDown()

	c.addOrUpdateSubnetQueue.ShutDown()
	c.deleteSubnetQueue.ShutDown()
	c.deleteRouteQueue.ShutDown()
	c.updateSubnetStatusQueue.ShutDown()
	c.syncVirtualPortsQueue.ShutDown()

	c.addNodeQueue.ShutDown()
	c.updateNodeQueue.ShutDown()
	c.deleteNodeQueue.ShutDown()

	c.addServiceQueue.ShutDown()
	c.deleteServiceQueue.ShutDown()
	c.updateServiceQueue.ShutDown()
	c.updateEndpointQueue.ShutDown()

	c.addVlanQueue.ShutDown()
	c.delVlanQueue.ShutDown()
	c.updateVlanQueue.ShutDown()

	c.updateProviderNetworkQueue.ShutDown()

	c.addOrUpdateVpcQueue.ShutDown()
	c.updateVpcStatusQueue.ShutDown()
	c.delVpcQueue.ShutDown()

	c.addOrUpdateVpcNatGatewayQueue.ShutDown()
	c.initVpcNatGatewayQueue.ShutDown()
	c.delVpcNatGatewayQueue.ShutDown()
	c.updateVpcEipQueue.ShutDown()
	c.updateVpcFloatingIpQueue.ShutDown()
	c.updateVpcDnatQueue.ShutDown()
	c.updateVpcSnatQueue.ShutDown()
	c.updateVpcSubnetQueue.ShutDown()

	if c.config.EnableLb {
		c.addSwitchLBRuleQueue.ShutDown()
		c.delSwitchLBRuleQueue.ShutDown()
		c.UpdateSwitchLBRuleQueue.ShutDown()

		c.addOrUpdateVpcDnsQueue.ShutDown()
		c.delVpcDnsQueue.ShutDown()
	}

	c.addVirtualIpQueue.ShutDown()
	c.updateVirtualIpQueue.ShutDown()
	c.delVirtualIpQueue.ShutDown()

	c.addIptablesEipQueue.ShutDown()
	c.updateIptablesEipQueue.ShutDown()
	c.resetIptablesEipQueue.ShutDown()
	c.delIptablesEipQueue.ShutDown()

	c.addIptablesFipQueue.ShutDown()
	c.updateIptablesFipQueue.ShutDown()
	c.delIptablesFipQueue.ShutDown()

	c.addIptablesDnatRuleQueue.ShutDown()
	c.updateIptablesDnatRuleQueue.ShutDown()
	c.delIptablesDnatRuleQueue.ShutDown()

	c.addIptablesSnatRuleQueue.ShutDown()
	c.updateIptablesSnatRuleQueue.ShutDown()
	c.delIptablesSnatRuleQueue.ShutDown()

	if c.config.EnableEipSnat {
		c.addOvnEipQueue.ShutDown()
		c.updateOvnEipQueue.ShutDown()
		c.resetOvnEipQueue.ShutDown()
		c.delOvnEipQueue.ShutDown()

		c.addOvnFipQueue.ShutDown()
		c.updateOvnFipQueue.ShutDown()
		c.delOvnFipQueue.ShutDown()

		c.addIptablesSnatRuleQueue.ShutDown()
		c.updateIptablesSnatRuleQueue.ShutDown()
		c.delIptablesSnatRuleQueue.ShutDown()
	}

	if c.config.PodDefaultFipType == util.IptablesFip {
		c.addPodAnnotatedIptablesEipQueue.ShutDown()
		c.updatePodAnnotatedIptablesEipQueue.ShutDown()
		c.delPodAnnotatedIptablesEipQueue.ShutDown()

		c.addPodAnnotatedIptablesFipQueue.ShutDown()
		c.updatePodAnnotatedIptablesFipQueue.ShutDown()
		c.delPodAnnotatedIptablesFipQueue.ShutDown()
	}
	if c.config.EnableNP {
		c.updateNpQueue.ShutDown()
		c.deleteNpQueue.ShutDown()
	}
	c.addOrUpdateSgQueue.ShutDown()
	c.delSgQueue.ShutDown()
	c.syncSgPortsQueue.ShutDown()
}

func (c *Controller) startWorkers(stopCh <-chan struct{}) {
	klog.Info("Starting workers")

	go wait.Until(c.runAddVpcWorker, time.Second, stopCh)

	go wait.Until(c.runAddOrUpdateVpcNatGwWorker, time.Second, stopCh)
	go wait.Until(c.runInitVpcNatGwWorker, time.Second, stopCh)
	go wait.Until(c.runDelVpcNatGwWorker, time.Second, stopCh)
	go wait.Until(c.runUpdateVpcFloatingIpWorker, time.Second, stopCh)
	go wait.Until(c.runUpdateVpcEipWorker, time.Second, stopCh)
	go wait.Until(c.runUpdateVpcDnatWorker, time.Second, stopCh)
	go wait.Until(c.runUpdateVpcSnatWorker, time.Second, stopCh)
	go wait.Until(c.runUpdateVpcSubnetWorker, time.Second, stopCh)

	// add default/join subnet and wait them ready
	go wait.Until(c.runAddSubnetWorker, time.Second, stopCh)
	go wait.Until(c.runAddVlanWorker, time.Second, stopCh)
	go wait.Until(c.runAddNamespaceWorker, time.Second, stopCh)
	for {
		klog.Infof("wait for %s and %s ready", c.config.DefaultLogicalSwitch, c.config.NodeSwitch)
		time.Sleep(3 * time.Second)
		lss, err := c.ovnLegacyClient.ListLogicalSwitch(c.config.EnableExternalVpc)
		if err != nil {
			util.LogFatalAndExit(err, "failed to list logical switch")
		}

		if util.IsStringIn(c.config.DefaultLogicalSwitch, lss) && util.IsStringIn(c.config.NodeSwitch, lss) && c.addNamespaceQueue.Len() == 0 {
			break
		}
	}

	go wait.Until(c.runAddSgWorker, time.Second, stopCh)
	go wait.Until(c.runDelSgWorker, time.Second, stopCh)
	go wait.Until(c.runSyncSgPortsWorker, time.Second, stopCh)

	// run node worker before handle any pods
	for i := 0; i < c.config.WorkerNum; i++ {
		go wait.Until(c.runAddNodeWorker, time.Second, stopCh)
		go wait.Until(c.runUpdateNodeWorker, time.Second, stopCh)
		go wait.Until(c.runDeleteNodeWorker, time.Second, stopCh)
	}
	for {
		ready := true
		time.Sleep(3 * time.Second)
		nodes, err := c.nodesLister.List(labels.Everything())
		if err != nil {
			util.LogFatalAndExit(err, "failed to list nodes")
		}
		for _, node := range nodes {
			if node.Annotations[util.AllocatedAnnotation] != "true" {
				klog.Infof("wait node %s annotation ready", node.Name)
				ready = false
				break
			}
		}
		if ready {
			break
		}
	}

	go wait.Until(c.runDelVpcWorker, time.Second, stopCh)
	go wait.Until(c.runUpdateVpcStatusWorker, time.Second, stopCh)
	go wait.Until(c.runUpdateProviderNetworkWorker, time.Second, stopCh)

	if c.config.EnableLb {
		go wait.Until(c.runAddServiceWorker, time.Second, stopCh)
		// run in a single worker to avoid delete the last vip, which will lead ovn to delete the loadbalancer
		go wait.Until(c.runDeleteServiceWorker, time.Second, stopCh)

		go wait.Until(c.runAddSwitchLBRuleWorker, time.Second, stopCh)
		go wait.Until(c.runDelSwitchLBRuleWorker, time.Second, stopCh)
		go wait.Until(c.runUpdateSwitchLBRuleWorker, time.Second, stopCh)

		go wait.Until(c.runAddOrUpdateVpcDnsWorker, time.Second, stopCh)
		go wait.Until(c.runDelVpcDnsWorker, time.Second, stopCh)
		go wait.Until(func() {
			c.resyncVpcDnsConfig()
		}, 5*time.Second, stopCh)
	}

	for i := 0; i < c.config.WorkerNum; i++ {
		go wait.Until(c.runAddPodWorker, time.Second, stopCh)
		go wait.Until(c.runDeletePodWorker, time.Second, stopCh)
		go wait.Until(c.runUpdatePodWorker, time.Second, stopCh)
		go wait.Until(c.runUpdatePodSecurityWorker, time.Second, stopCh)

		go wait.Until(c.runDeleteSubnetWorker, time.Second, stopCh)
		go wait.Until(c.runDeleteRouteWorker, time.Second, stopCh)
		go wait.Until(c.runUpdateSubnetStatusWorker, time.Second, stopCh)
		go wait.Until(c.runSyncVirtualPortsWorker, time.Second, stopCh)

		if c.config.EnableLb {
			go wait.Until(c.runUpdateServiceWorker, time.Second, stopCh)
			go wait.Until(c.runUpdateEndpointWorker, time.Second, stopCh)
		}

		if c.config.EnableNP {
			go wait.Until(c.runUpdateNpWorker, time.Second, stopCh)
			go wait.Until(c.runDeleteNpWorker, time.Second, stopCh)
		}

		go wait.Until(c.runDelVlanWorker, time.Second, stopCh)
		go wait.Until(c.runUpdateVlanWorker, time.Second, stopCh)
	}

	go wait.Until(func() {
		c.resyncInterConnection()
	}, time.Second, stopCh)

	go wait.Until(func() {
		c.SynRouteToPolicy()
	}, 5*time.Second, stopCh)

	go wait.Until(func() {
		c.resyncExternalGateway()
	}, time.Second, stopCh)

	go wait.Until(func() {
		c.resyncVpcNatGwConfig()
	}, time.Second, stopCh)

	go wait.Until(func() {
		if err := c.markAndCleanLSP(); err != nil {
			klog.Errorf("gc lsp error: %v", err)
		}
	}, time.Duration(c.config.GCInterval)*time.Second, stopCh)

	go wait.Until(func() {
		if err := c.inspectPod(); err != nil {
			klog.Errorf("inspection error: %v", err)
		}
	}, time.Duration(c.config.InspectInterval)*time.Second, stopCh)

	if c.config.EnableExternalVpc {
		go wait.Until(func() {
			c.syncExternalVpc()
		}, 5*time.Second, stopCh)
	}

	go wait.Until(c.resyncProviderNetworkStatus, 30*time.Second, stopCh)
	go wait.Until(c.resyncSubnetMetrics, 30*time.Second, stopCh)
	go wait.Until(c.CheckGatewayReady, 5*time.Second, stopCh)

	if c.config.EnableEipSnat {
		go wait.Until(c.runAddOvnEipWorker, time.Second, stopCh)
		go wait.Until(c.runUpdateOvnEipWorker, time.Second, stopCh)
		go wait.Until(c.runResetOvnEipWorker, time.Second, stopCh)
		go wait.Until(c.runDelOvnEipWorker, time.Second, stopCh)

		go wait.Until(c.runAddOvnFipWorker, time.Second, stopCh)
		go wait.Until(c.runUpdateOvnFipWorker, time.Second, stopCh)
		go wait.Until(c.runDelOvnFipWorker, time.Second, stopCh)

		go wait.Until(c.runAddOvnSnatRuleWorker, time.Second, stopCh)
		go wait.Until(c.runUpdateOvnSnatRuleWorker, time.Second, stopCh)
		go wait.Until(c.runDelOvnSnatRuleWorker, time.Second, stopCh)
	}

	if c.config.EnableNP {
		go wait.Until(c.CheckNodePortGroup, time.Duration(c.config.NodePgProbeTime)*time.Minute, stopCh)
	}

	go wait.Until(c.syncVmLiveMigrationPort, 15*time.Second, stopCh)

	go wait.Until(c.runAddVirtualIpWorker, time.Second, stopCh)
	go wait.Until(c.runUpdateVirtualIpWorker, time.Second, stopCh)
	go wait.Until(c.runDelVirtualIpWorker, time.Second, stopCh)

	go wait.Until(c.runAddIptablesEipWorker, time.Second, stopCh)
	go wait.Until(c.runUpdateIptablesEipWorker, time.Second, stopCh)
	go wait.Until(c.runResetIptablesEipWorker, time.Second, stopCh)
	go wait.Until(c.runDelIptablesEipWorker, time.Second, stopCh)

	go wait.Until(c.runAddIptablesFipWorker, time.Second, stopCh)
	go wait.Until(c.runUpdateIptablesFipWorker, time.Second, stopCh)
	go wait.Until(c.runDelIptablesFipWorker, time.Second, stopCh)

	go wait.Until(c.runAddIptablesDnatRuleWorker, time.Second, stopCh)
	go wait.Until(c.runUpdateIptablesDnatRuleWorker, time.Second, stopCh)
	go wait.Until(c.runDelIptablesDnatRuleWorker, time.Second, stopCh)

	go wait.Until(c.runAddIptablesSnatRuleWorker, time.Second, stopCh)
	go wait.Until(c.runUpdateIptablesSnatRuleWorker, time.Second, stopCh)
	go wait.Until(c.runDelIptablesSnatRuleWorker, time.Second, stopCh)

	if c.config.PodDefaultFipType == util.IptablesFip {
		go wait.Until(c.runAddPodAnnotatedIptablesEipWorker, time.Second, stopCh)
		go wait.Until(c.runDelPodAnnotatedIptablesEipWorker, time.Second, stopCh)

		go wait.Until(c.runAddPodAnnotatedIptablesFipWorker, time.Second, stopCh)
		go wait.Until(c.runDelPodAnnotatedIptablesFipWorker, time.Second, stopCh)
	}
}
