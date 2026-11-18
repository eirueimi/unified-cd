package dsl

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"

	"github.com/eirueimi/unified-cd/internal/imageref"
)

// imageTemplateData is the only data an image template can see. It is a
// dedicated struct (not TemplateData) so .Secrets, .Steps, .Matrix and .Foreach
// fail as errors instead of silently expanding to nothing.
type imageTemplateData struct {
	Params map[string]string
	Vars   map[string]string
}

func parseImageTemplate(tpl string) (*template.Template, error) {
	return template.New("").Funcs(funcMap).Option("missingkey=error").Parse(tpl)
}

// ValidateImageTemplate checks an image field that contains a template: it
// must parse as a Go template. The expanded value is only known at claim time,
// so imageref.Validate is not applied here. A string without "{{" is validated
// as a plain image reference.
func ValidateImageTemplate(s string) error {
	if !strings.Contains(s, "{{") {
		return imageref.Validate(s)
	}
	if _, err := parseImageTemplate(s); err != nil {
		return fmt.Errorf("parse image template: %w", err)
	}
	return nil
}

// ExpandImage expands {{ .Params.X }} / {{ .Vars.X }} in a container image
// field. A string without "{{" is returned untouched. Unlike run: templates it
// is strict: a missing key, an unknown field (.Secrets, .Steps, ...) or a parse
// error is an error, and the expanded result must pass imageref.Validate.
func ExpandImage(tpl string, params, vars map[string]string) (string, error) {
	if !strings.Contains(tpl, "{{") {
		return tpl, nil
	}
	t, err := parseImageTemplate(tpl)
	if err != nil {
		return "", fmt.Errorf("parse image template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, imageTemplateData{Params: params, Vars: vars}); err != nil {
		return "", fmt.Errorf("expand image template: %w", err)
	}
	out := buf.String()
	if err := imageref.Validate(out); err != nil {
		return "", fmt.Errorf("expanded image is invalid: %w", err)
	}
	return out, nil
}

// mapContainerImages applies fn to the image of every container in list whose
// image contains a template. Inputs are never mutated: when something changes
// it returns a new slice in which only the rewritten container maps are copies.
// changed reports whether the returned slice differs from list.
func mapContainerImages(list []map[string]any, label string, fn func(string) (string, error)) (out []map[string]any, changed bool, err error) {
	out = list
	for i, c := range list {
		img, ok := c["image"].(string)
		if !ok || !strings.Contains(img, "{{") {
			continue
		}
		res, ferr := fn(img)
		if ferr != nil {
			return nil, false, fmt.Errorf("%s %q image %q: %w", label, DefName(c), img, ferr)
		}
		if !changed {
			out = append([]map[string]any(nil), list...)
			changed = true
		}
		nc := make(map[string]any, len(c))
		for k, v := range c {
			nc[k] = v
		}
		nc["image"] = res
		out[i] = nc
	}
	return out, changed, nil
}

func mapPodTemplateImages(pt *PodTemplate, fn func(string) (string, error)) (*PodTemplate, error) {
	if pt == nil {
		return nil, nil
	}
	out := *pt
	for _, key := range []string{"containers", "initContainers"} {
		raw, _ := pt.Spec[key].([]any)
		defs := make([]map[string]any, 0, len(raw))
		idx := make([]int, 0, len(raw))
		for i, r := range raw {
			if d, ok := r.(map[string]any); ok {
				defs = append(defs, d)
				idx = append(idx, i)
			}
		}
		label := "podTemplate container"
		if key == "initContainers" {
			label = "podTemplate initContainer"
		}
		nd, changed, err := mapContainerImages(defs, label, fn)
		if err != nil {
			return nil, err
		}
		if !changed {
			continue
		}
		nl := append([]any(nil), raw...)
		for j, i := range idx {
			nl[i] = nd[j]
		}
		spec := make(map[string]any, len(pt.Spec))
		for k, v := range out.Spec {
			spec[k] = v
		}
		spec[key] = nl
		out.Spec = spec
	}
	if pt.Override != nil {
		nd, changed, err := mapContainerImages(pt.Override.Containers, "podTemplate.override container", fn)
		if err != nil {
			return nil, err
		}
		if changed {
			patch := *pt.Override
			patch.Containers = nd
			out.Override = &patch
		}
	}
	return &out, nil
}

// ExpandPodTemplateImages returns pt with every templated container image
// (containers, initContainers, override containers) expanded by ExpandImage.
// pt is never mutated; only the parts that change are copied. Errors name the
// container and the unexpanded image.
func ExpandPodTemplateImages(pt *PodTemplate, params, vars map[string]string) (*PodTemplate, error) {
	return mapPodTemplateImages(pt, func(img string) (string, error) {
		return ExpandImage(img, params, vars)
	})
}

// validatePodTemplateImages checks, at apply time, that every templated
// container image parses as a Go template. Plain images are not checked here
// (unchanged behaviour: the runtime validates them).
func validatePodTemplateImages(pt *PodTemplate) error {
	_, err := mapPodTemplateImages(pt, func(img string) (string, error) {
		return img, ValidateImageTemplate(img)
	})
	return err
}
