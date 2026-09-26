package mcp

// SubmitTaskInput defines input arguments for the task_submit tool.
type SubmitTaskInput struct {
	NodeID         string `json:"node_id" jsonschema:"Target node identifier where the task should execute"`
	Instruction    string `json:"instruction" jsonschema:"The instruction or command text for the agent task"`
	Capability     string `json:"capability,omitempty" jsonschema:"Capability name (defaults to agent.run)"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"Task timeout in seconds (default 120)"`
}

// SubmitTaskOutput represents the outcome of task_submit.
type SubmitTaskOutput struct {
	TaskID    string `json:"task_id"`
	NodeID    string `json:"node_id"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
	Message   string `json:"message"`
}

// GetTaskInput defines input arguments for the task_get tool.
type GetTaskInput struct {
	TaskID string `json:"task_id" jsonschema:"The unique task ID to inspect"`
}

// GetTaskOutput represents the task detail and execution result.
type GetTaskOutput struct {
	TaskID    string `json:"task_id"`
	NodeID    string `json:"node_id"`
	State     string `json:"state"`
	AttemptID string `json:"attempt_id,omitempty"`
	Output    string `json:"output,omitempty"`
	ExitCode  int    `json:"exit_code,omitempty"`
	Error     string `json:"error,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// CancelTaskInput defines input arguments for the task_cancel tool.
type CancelTaskInput struct {
	TaskID string `json:"task_id" jsonschema:"The unique task ID to cancel"`
}

// CancelTaskOutput represents the outcome of task_cancel.
type CancelTaskOutput struct {
	TaskID  string `json:"task_id"`
	State   string `json:"state"`
	Message string `json:"message"`
}

// ListDevicesInput defines input arguments for device_list.
type ListDevicesInput struct{}

// DeviceItem represents summary of one enrolled node.
type DeviceItem struct {
	NodeID      string `json:"node_id"`
	Fingerprint string `json:"fingerprint"`
	Revoked     bool   `json:"revoked"`
	ExpiresAt   string `json:"expires_at"`
}

// ListDevicesOutput lists all enrolled nodes.
type ListDevicesOutput struct {
	Devices []DeviceItem `json:"devices"`
}

// DiagnoseInput defines input arguments for doctor_diagnose.
type DiagnoseInput struct{}

// CheckItem represents one health diagnostic result.
type CheckItem struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

// DiagnoseOutput represents the overall doctor report.
type DiagnoseOutput struct {
	Healthy bool        `json:"healthy"`
	Checks  []CheckItem `json:"checks"`
}
