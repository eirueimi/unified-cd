package dsl

import (
	"fmt"
	"io"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// cronParser accepts only the 5-field format (minute hour day month weekday).
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// ParseSchedule decodes and validates a Schedule YAML from an io.Reader.
func ParseSchedule(r io.Reader) (*Schedule, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var s Schedule
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("decode yaml: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// Validate validates the required fields and cron expression of a Schedule.
func (s *Schedule) Validate() error {
	if s.APIVersion != SupportedAPIVersion {
		return fmt.Errorf("unsupported apiVersion %q", s.APIVersion)
	}
	if s.Kind != "Schedule" {
		return fmt.Errorf("unsupported kind %q (want \"Schedule\")", s.Kind)
	}
	if err := ValidateName(s.Metadata.Name); err != nil {
		return fmt.Errorf("metadata.name %w", err)
	}
	if s.Spec.Cron == "" {
		return fmt.Errorf("spec.cron is required")
	}
	if _, err := cronParser.Parse(s.Spec.Cron); err != nil {
		return fmt.Errorf("spec.cron is invalid: %w", err)
	}
	if s.Spec.Job == "" {
		return fmt.Errorf("spec.job is required")
	}
	return nil
}

// NextCronTime returns the next fire time after after for the given cron expression.
// Returns an error if the cron expression is invalid.
func NextCronTime(expr string, after time.Time) (time.Time, error) {
	sched, err := cronParser.Parse(expr)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid cron expression: %w", err)
	}
	return sched.Next(after), nil
}
