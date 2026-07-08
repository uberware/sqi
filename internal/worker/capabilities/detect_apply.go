// SPDX-License-Identifier: AGPL-3.0-or-later

package capabilities

import "fmt"

// LoadDetectors returns the active detectors: built-ins not named in cfg.Disable,
// followed by validated custom detectors from cfg.Detect.
func LoadDetectors(cfg CapabilitiesConfig) ([]Detector, error) {
	builtins, err := BuiltinDetectors()
	if err != nil {
		return nil, fmt.Errorf("load builtin detectors: %w", err)
	}
	disabled := map[string]bool{}
	for _, tag := range cfg.Disable {
		disabled[tag] = true
	}
	var out []Detector
	for _, d := range builtins {
		if !disabled[d.Tag] {
			out = append(out, d)
		}
	}
	for i, d := range cfg.Detect {
		if err := d.Validate(); err != nil {
			return nil, fmt.Errorf("custom detector %d: %w", i, err)
		}
		d.Origin = "custom"
		out = append(out, d)
	}
	return out, nil
}

// ApplyDetectors evaluates each detector against env and records emitted tags as
// presence tags (empty value) without overwriting keys already present.
func (c *Capabilities) ApplyDetectors(detectors []Detector, env CheckEnv) {
	if c.Tags == nil {
		c.Tags = make(map[string]string)
	}
	for _, d := range detectors {
		for _, tag := range d.Evaluate(env) {
			if _, exists := c.Tags[tag]; !exists {
				c.Tags[tag] = ""
			}
		}
	}
}

// BuildWorkerCapabilities detects host capabilities, applies built-in + custom
// detectors, merges manual tags (which win), and validates the tag key set.
func BuildWorkerCapabilities(cfg CapabilitiesConfig, manualTags []string, env CheckEnv) (Capabilities, error) {
	caps := Detect(nil)
	detectors, err := LoadDetectors(cfg)
	if err != nil {
		return Capabilities{}, err
	}
	caps.ApplyDetectors(detectors, env)
	caps.MergeManualTags(manualTags)
	if err := ValidateTagKeys(caps.Tags); err != nil {
		return Capabilities{}, fmt.Errorf("validate capability tag keys: %w", err)
	}
	return caps, nil
}
