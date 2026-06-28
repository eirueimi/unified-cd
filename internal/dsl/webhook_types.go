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

type WebhookTrigger struct {
	Job string `yaml:"job"`
}

type WebhookAuth struct {
	Type      string `yaml:"type" schema:"enum:none,hmac-sha256,github"` // none | hmac-sha256 | github (X-Hub-Signature-256)
	SecretRef string `yaml:"secretRef,omitempty"`
}
