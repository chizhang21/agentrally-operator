package controller

// +kubebuilder:rbac:groups=arally.io,resources=agentdefinitions,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=arally.io,resources=agentdefinitions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=arally.io,resources=agentdefinitions/finalizers,verbs=update

// +kubebuilder:rbac:groups=arally.io,resources=agentgroups,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=arally.io,resources=agentinstances,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=arally.io,resources=agentgroups/status;agentinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=arally.io,resources=agentgroups/finalizers;agentinstances/finalizers,verbs=update
// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=core,resources=namespaces;resourcequotas;limitranges,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;create;update;patch;delete
// Secrets are read by explicit reference; no global Secret list, watch or cache.
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch
// Event reads are UID-filtered diagnostics; the Operator never watches all Events.
// +kubebuilder:rbac:groups=core,resources=events,verbs=list;create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch,namespace=system
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;patch;delete

// Capability discovery only reads native resources. It creates the default report
// when absent, preserving spec/metadata on existing reports.
// +kubebuilder:rbac:groups=arally.io,resources=clustercapabilities,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=arally.io,resources=clustercapabilities/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=nodes;configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=node.k8s.io,resources=runtimeclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses;csidrivers;csinodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=cilium.io,resources=ciliumnodeconfigs,verbs=list

// Pods are observed only; Core retains all Pod lifecycle writes.
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get
// ServiceAccounts are administrator-managed; the Operator only verifies references.
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get
// Allocation and managed pool setup use upstream CRs; Pod/refill lifecycle stays upstream.
// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxclaims,verbs=get;list;create;delete
// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools,verbs=get;list;create;patch;delete
// Template metadata coordinates allocation with pruning; snapshot specs stay immutable.
// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxtemplates,verbs=get;list;watch;create;patch;delete
