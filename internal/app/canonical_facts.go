package app

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// CanonicalFacts is the secret-free effective-state projection supplied by a
// target observer. The authoring model remains closed: these output-only facts
// can be rendered by canonical/status callers but cannot be written in ob.yml.
type CanonicalFacts struct {
	Hygiene  CanonicalHygieneFacts            `json:"hygiene"`
	Services map[string]CanonicalServiceFacts `json:"services,omitempty"`
}

type CanonicalHygieneFacts struct {
	LoggingDriver   CanonicalFact `json:"logging_driver"`
	LoggingMaxSize  CanonicalFact `json:"logging_max_size"`
	LoggingMaxFiles CanonicalFact `json:"logging_max_files"`
}

type CanonicalServiceFacts struct {
	ProtectionState        CanonicalFact               `json:"protection_state"`
	Tier                   CanonicalFact               `json:"tier"`
	RecoveryKind           CanonicalFact               `json:"recovery_kind"`
	ServiceImageDigest     CanonicalFact               `json:"service_image_digest"`
	EncryptionMode         CanonicalFact               `json:"encryption_mode"`
	ObservedRPO            CanonicalFact               `json:"observed_rpo"`
	ObservedRecoveryWindow CanonicalFact               `json:"observed_recovery_window"`
	ExpectedInterruption   CanonicalFact               `json:"expected_interruption"`
	DrillCapacityState     CanonicalFact               `json:"drill_capacity_state"`
	Prerequisites          []CanonicalPrerequisiteFact `json:"prerequisites,omitempty"`
}

// CanonicalFact carries one value and the authority for that value. Values in
// this contract are bounded operational metadata, never free-form output from
// a helper or a secret-bearing URL.
type CanonicalFact struct {
	Value  string `json:"value"`
	Origin Origin `json:"origin"`
}

type CanonicalPrerequisiteFact struct {
	Code   string `json:"code"`
	State  string `json:"state"`
	Origin Origin `json:"origin"`
}

var canonicalDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// WithCanonicalFacts returns a shallow resolved-project copy carrying
// effective output facts. It does not mutate the reusable resolved project.
func (r *Resolved) WithCanonicalFacts(facts CanonicalFacts) *Resolved {
	clone := *r
	clone.canonicalFacts = &facts
	return &clone
}

func canonicalFactsGeneric(facts CanonicalFacts) (map[string]any, error) {
	if err := validateCanonicalFacts(facts); err != nil {
		return nil, err
	}
	body, err := json.Marshal(facts)
	if err != nil {
		return nil, fmt.Errorf("encode canonical facts: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode canonical facts: %w", err)
	}
	return out, nil
}

func validateCanonicalFacts(facts CanonicalFacts) error {
	if err := validateFact("hygiene.logging_driver", facts.Hygiene.LoggingDriver, nil); err != nil {
		return err
	}
	if err := validateFact("hygiene.logging_max_size", facts.Hygiene.LoggingMaxSize, gSize.pattern); err != nil {
		return err
	}
	if err := validateFact("hygiene.logging_max_files", facts.Hygiene.LoggingMaxFiles, regexp.MustCompile(`^[1-9][0-9]*$`)); err != nil {
		return err
	}
	for _, name := range sortedKeys(facts.Services) {
		if err := gIdent.check("effective.services."+name, name); err != nil {
			return err
		}
		service := facts.Services[name]
		checks := []struct {
			field string
			fact  CanonicalFact
			valid *regexp.Regexp
		}{
			{"protection_state", service.ProtectionState, enumPattern("undeclared", "declared", "enabled", "disable-pending", "disabled")},
			{"tier", service.Tier, enumPattern("Run", "Managed", "External")},
			{"recovery_kind", service.RecoveryKind, enumPattern(eRecoveryKind...)},
			{"service_image_digest", service.ServiceImageDigest, canonicalDigest},
			{"encryption_mode", service.EncryptionMode, enumPattern(eEncryptionMode...)},
			{"observed_rpo", service.ObservedRPO, durationPattern()},
			{"observed_recovery_window", service.ObservedRecoveryWindow, durationPattern()},
			{"expected_interruption", service.ExpectedInterruption, enumOrDurationPattern("none")},
			{"drill_capacity_state", service.DrillCapacityState, enumPattern("available", "admitted", "deferred", "unobserved")},
		}
		for _, check := range checks {
			if err := validateFact("services."+name+"."+check.field, check.fact, check.valid); err != nil {
				return err
			}
		}
		for i, prerequisite := range service.Prerequisites {
			path := fmt.Sprintf("services.%s.prerequisites[%d]", name, i)
			if !gIdent.pattern.MatchString(prerequisite.Code) {
				return fmt.Errorf("%s.code is not safe canonical metadata", path)
			}
			if !enumPattern("met", "missing", "drifted", "unobserved").MatchString(prerequisite.State) {
				return fmt.Errorf("%s.state is not a supported prerequisite state", path)
			}
			if !canonicalFactOrigin(prerequisite.Origin) {
				return fmt.Errorf("%s.origin is not a canonical origin", path)
			}
		}
	}
	return nil
}

func validateFact(path string, fact CanonicalFact, valid *regexp.Regexp) error {
	if !canonicalFactOrigin(fact.Origin) {
		return fmt.Errorf("%s.origin is not a canonical origin", path)
	}
	if fact.Value == "" {
		return fmt.Errorf("%s.value is required", path)
	}
	if valid != nil && !valid.MatchString(fact.Value) {
		// Deliberately omit the value: invalid observed metadata may contain a
		// credential-bearing URL or helper output and must not be reflected.
		return fmt.Errorf("%s.value is not safe canonical metadata", path)
	}
	return nil
}

func canonicalFactOrigin(origin Origin) bool {
	switch origin {
	case OriginAuthored, OriginDefault, OriginEnvironmentOverride, OriginObserved, OriginDerived:
		return true
	default:
		return false
	}
}

func enumPattern(values ...string) *regexp.Regexp {
	joined := ""
	for i, value := range values {
		if i > 0 {
			joined += "|"
		}
		joined += regexp.QuoteMeta(value)
	}
	return regexp.MustCompile("^(" + joined + ")$")
}

func durationPattern() *regexp.Regexp {
	return gDur.pattern
}

func enumOrDurationPattern(values ...string) *regexp.Regexp {
	joined := ""
	for i, value := range values {
		if i > 0 {
			joined += "|"
		}
		joined += regexp.QuoteMeta(value)
	}
	duration := gDur.pattern.String()
	duration = duration[1 : len(duration)-1]
	if joined != "" {
		joined += "|"
	}
	return regexp.MustCompile("^(" + joined + duration + ")$")
}
