package dsl

// Schedule is the DSL type for a cron schedule trigger.
type Schedule struct {
	APIVersion string       `yaml:"apiVersion"`
	Kind       string       `yaml:"kind"`
	Metadata   Metadata     `yaml:"metadata"`
	Spec       ScheduleSpec `yaml:"spec"`
}

// ScheduleSpec is the spec section of Schedule.
type ScheduleSpec struct {
	Cron   string            `yaml:"cron"`
	Job    string            `yaml:"job"`
	Params map[string]string `yaml:"params,omitempty"`
}
