package control

import "time"

// TemporaryDiscoveryCandidate binds a read-only, qualified Ollama installation.
// It never selects a model or modifies the accepted workload catalog.
type TemporaryDiscoveryCandidate struct {
	GPUUUID        string         `json:"gpuUUID,omitempty"`
	Unit           string         `json:"unit"`
	LaunchFile     string         `json:"launchFile"`
	DropIns        []LaunchSource `json:"dropIns,omitempty"`
	LaunchSHA256   string         `json:"launchSHA256"`
	Endpoint       string         `json:"endpoint"`
	Cgroup         string         `json:"cgroup"`
	SystemdSlice   string         `json:"systemdSlice"`
	SystemdVersion uint16         `json:"systemdVersion"`
}

type TemporaryDiscoverySession struct {
	ID                    string                           `json:"id"`
	Token                 string                           `json:"token"`
	ConfigurationRevision string                           `json:"configurationRevision"`
	Fence                 Fence                            `json:"fence"`
	Version               uint64                           `json:"version"`
	Candidate             TemporaryDiscoveryCandidate      `json:"candidate"`
	PriorStopped          bool                             `json:"priorStopped"`
	LaunchEvidence        TemporaryDiscoveryLaunchEvidence `json:"launchEvidence"`
	InvocationID          string                           `json:"invocationId,omitempty"`
	Status                string                           `json:"status"`
	Error                 string                           `json:"error,omitempty"`
	UpdatedAt             time.Time                        `json:"updatedAt"`
}

// TemporaryDiscoveryLaunchEvidence records the accepted anchor job and fresh
// invocation. External account control must remain paused during the session.
type TemporaryDiscoveryLaunchEvidence struct {
	InvocationID        string `json:"invocationId"`
	JobID               string `json:"jobId"`
	ActivationTimestamp string `json:"activationTimestamp"`
	PriorInvocationID   string `json:"priorInvocationId"`
}
