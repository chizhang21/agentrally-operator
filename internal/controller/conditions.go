package controller

// Condition types shared by AgentRally's controllers and cleanup paths.
// Core observations are validated against the current execution before mirroring.
const (
	ConditionReady                 = "Ready"
	ConditionAvailable             = "Available"
	ConditionConfigurationAccepted = "ConfigurationAccepted"
	ConditionConfigurationApplied  = "ConfigurationApplied"
	ConditionProvisioningReady     = "ProvisioningReady"
	ConditionInteractionReady      = "InteractionReady"
	ConditionCapabilityValidated   = "CapabilityValidated"
	ConditionRestarting            = "Restarting"
	ConditionExpired               = "Expired"
	ConditionPodScheduled          = "PodScheduled"
	ConditionFinished              = "Finished"
	ConditionSuspended             = "Suspended"
	ConditionCleanupReady          = "CleanupReady"
	ConditionActivityReady         = "ActivityReady"
)

// Transition reasons also consumed by event classification and state checks.
const (
	ReasonRestarting           = "Restarting"
	ReasonRestartCompleted     = "RestartCompleted"
	ReasonExpired              = "Expired"
	ReasonPoolPending          = "PoolPending"
	ReasonPoolAllocated        = "PoolAllocated"
	ReasonPoolClaimTimeout     = "PoolClaimTimeout"
	ReasonAllocationSuspended  = "AllocationSuspended"
	ReasonGroupNotReady        = "GroupNotReady"
	ReasonDependenciesNotReady = "DependenciesNotReady"
)
