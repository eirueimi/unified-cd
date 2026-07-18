package dsl

// WebhookReceiver is the DSL type for webhook receiver configuration.
type WebhookReceiver struct {
	APIVersion string              `yaml:"apiVersion"`
	Kind       string              `yaml:"kind"`
	Metadata   Metadata            `yaml:"metadata"`
	Spec       WebhookReceiverSpec `yaml:"spec"`
}

type WebhookReceiverSpec struct {
	Trigger       WebhookTrigger    `yaml:"trigger"`
	Auth          WebhookAuth       `yaml:"auth"`
	ParamsMapping map[string]string `yaml:"paramsMapping,omitempty"`
	Filters       []string          `yaml:"filters,omitempty"`
}

// WebhookTrigger selects what a webhook delivery triggers. Exactly one of Job or
// AppSource must be set: Job creates a Run; AppSource forces a GitOps re-sync of
// the named AppSource on the next reconciler tick.
type WebhookTrigger struct {
	Job       string `yaml:"job,omitempty"`
	AppSource string `yaml:"appSource,omitempty"`
}

type WebhookAuth struct {
	Type                 string `yaml:"type" schema:"enum:none,hmac-sha256,github,token"` // none | hmac-sha256 | github | token
	SecretRef            string `yaml:"secretRef,omitempty"`
	Header               string `yaml:"header,omitempty"`               // token type only: header to compare (default X-Gitlab-Token)
	AllowUnauthenticated bool   `yaml:"allowUnauthenticated,omitempty"` // required alongside type: none — makes an unauthenticated webhook a deliberate, greppable opt-in
}
