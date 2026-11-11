// SPDX-License-Identifier: AGPL-3.0-or-later

// Package scenario defines the load harness's scenario file: the YAML schema,
// its defaults, its validation and the --set overrides applied on top of it.
package scenario

// TargetKind says what the harness drives. Some fields only make sense for one.
type TargetKind int

const (
	// TargetManaged starts and owns an sqi-server for the run.
	TargetManaged TargetKind = iota
	// TargetAttach drives an sqi-server somebody else is running.
	TargetAttach
)

// Scenario is one load or soak run.
type Scenario struct {
	Name         string              `yaml:"name"`
	Description  string              `yaml:"description"`
	Duration     Duration            `yaml:"duration"`
	Warmup       Duration            `yaml:"warmup"`
	DrainTimeout Duration            `yaml:"drain_timeout"`
	Server       ServerSpec          `yaml:"server"`
	Farm         FarmSpec            `yaml:"farm"`
	Workers      []WorkerGroup       `yaml:"workers"`
	Jobs         []JobClass          `yaml:"jobs"`
	JobDeps      float64             `yaml:"job_deps"`
	Clients      ClientsSpec         `yaml:"clients"`
	Actions      []Action            `yaml:"actions"`
	Assert       map[string]Duration `yaml:"assert"`
	Faults       []Fault             `yaml:"faults,omitempty"`
	Expect       Expect              `yaml:"expect,omitempty"`
}

// ServerSpec configures the server a managed target starts.
type ServerSpec struct {
	Store      string         `yaml:"store"` // "sqlite" or "postgres"
	BrokerAuth bool           `yaml:"broker_auth"`
	HTTPAuth   bool           `yaml:"http_auth"`
	TLS        bool           `yaml:"tls"`
	Config     map[string]any `yaml:"config"` // merged over the harness's settings; harness-owned and unknown keys are rejected
}

// FarmSpec declares the queues and usage pools the run creates.
type FarmSpec struct {
	Queues     []QueueSpec `yaml:"queues"`
	UsagePools []PoolSpec  `yaml:"usage_pools"`
}

// QueueSpec is one queue.
type QueueSpec struct {
	Name               string `yaml:"name"`
	MaxConcurrentTasks int    `yaml:"max_concurrent_tasks"`
}

// PoolSpec is one usage pool.
type PoolSpec struct {
	Name  string `yaml:"name"`
	Limit int    `yaml:"limit"`
}

// Command is the onRun command the driver writes into every template: the
// no-op binary real workers run, or a placeholder no worker executes.
type Command struct {
	Path string
	Args []string
}

// WorkerGroup is a set of identically configured workers.
type WorkerGroup struct {
	Name      string            `yaml:"name"`
	Count     int               `yaml:"count"` // simulated workers
	Real      int               `yaml:"real"`  // real sqi-worker processes (managed target only)
	Ramp      Rate              `yaml:"ramp"`  // connect and register rate
	CPU       int               `yaml:"cpu"`
	RAMMb     int               `yaml:"ram_mb"`
	OS        string            `yaml:"os"`
	Tags      map[string]string `yaml:"tags"`
	Queues    []string          `yaml:"queues"` // empty means every queue
	Heartbeat Duration          `yaml:"heartbeat"`
	Churn     Churn             `yaml:"churn"`
}

// WorkerCounts is the size of the simulated fleet and the number of real
// sqi-worker processes, each summed over the worker groups.
func (s *Scenario) WorkerCounts() (simulated, processes int) {
	for _, g := range s.Workers {
		simulated += g.Count
		processes += g.Real
	}
	return simulated, processes
}

// Churn is how often a worker in the group crashes or loses its connection.
type Churn struct {
	CrashPerHour      float64  `yaml:"crash_per_hour"`
	DisconnectPerHour float64  `yaml:"disconnect_per_hour"`
	RestartAfter      Duration `yaml:"restart_after"`
}

// JobClass is a family of jobs submitted at a rate, or in a burst of Total.
type JobClass struct {
	Name         string   `yaml:"name"`
	Rate         Rate     `yaml:"rate"`
	Total        int      `yaml:"total"`
	Queue        string   `yaml:"queue"`
	Steps        int      `yaml:"steps"` // step i+1 depends on step i
	TasksPerStep int      `yaml:"tasks_per_step"`
	Chunk        int      `yaml:"chunk"` // frames per task
	Cores        int      `yaml:"cores"` // 0 means the whole worker
	UsagePool    string   `yaml:"usage_pool"`
	EXPR         bool     `yaml:"expr"`
	Task         TaskSpec `yaml:"task"`
	// RealTask changes what the no-op command does on a real worker; nil keeps
	// the default.
	RealTask *RealTask `yaml:"real_task,omitempty"`
}

// TaskSpec describes how a simulated worker runs the tasks of a job class.
type TaskSpec struct {
	Duration     Dist    `yaml:"duration"`
	FailRate     float64 `yaml:"fail_rate"` // each attempt fails with this probability
	HangRate     float64 `yaml:"hang_rate"` // runs until canceled or timed out
	LogLines     Dist    `yaml:"log_lines"`
	LogLineBytes int     `yaml:"log_line_bytes"`
	// ProgressEvery is how often a running task writes an openjd_progress
	// line; the worker's own throttle decides how many become snapshots.
	// 0 writes none.
	ProgressEvery Duration `yaml:"progress_every"`
}

// ClientsSpec is the read-side load: browsers and scripts watching the farm.
type ClientsSpec struct {
	WebSocket   WSSpec   `yaml:"websocket"`
	RESTPollers PollSpec `yaml:"rest_pollers"`
}

// WSSpec is the WebSocket clients.
type WSSpec struct {
	Count     int      `yaml:"count"`
	Subscribe []string `yaml:"subscribe"`
}

// PollSpec is the REST pollers.
type PollSpec struct {
	Count     int      `yaml:"count"`
	Interval  Duration `yaml:"interval"`
	Endpoints []string `yaml:"endpoints"`
}

// Action is a timed event during the run. It does at least one thing.
type Action struct {
	At             Duration  `yaml:"at"`
	CancelJobs     *Fraction `yaml:"cancel_jobs"`
	RetryFailed    bool      `yaml:"retry_failed"`
	PauseQueue     string    `yaml:"pause_queue"`
	ResumeQueue    string    `yaml:"resume_queue"`
	DisableWorkers *Fraction `yaml:"disable_workers"`
}

// Fraction is a share of some population, in [0, 1].
type Fraction struct {
	Fraction float64 `yaml:"fraction"`
}
