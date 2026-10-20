package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/zekihan/mailvault/internal/config"
	"github.com/zekihan/mailvault/internal/plugins/builtin"
)

type validationResource struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	host   string
	port   int
}

type validationReport struct {
	Valid              bool                 `json:"valid"`
	ConfigurationValid bool                 `json:"configuration_valid"`
	ConnectivityTested bool                 `json:"connectivity_tested"`
	Sources            []validationResource `json:"sources"`
	Targets            []validationResource `json:"targets"`
	Pairs              int                  `json:"pairs"`
	Errors             []string             `json:"errors"`
	mappings           []config.PairConfig
}

func validateConfiguration(contexts ...context.Context) (report validationReport, err error) {
	report.Sources = []validationResource{}
	report.Targets = []validationResource{}
	report.Errors = []string{}
	defer func() {
		if err != nil {
			report.Valid = false
			if len(report.Errors) == 0 {
				report.Errors = append(report.Errors, err.Error())
			}
		}
	}()
	cfg, err := config.LoadForValidation(cfgFile)
	if err != nil {
		return report, fmt.Errorf("load config: %w", err)
	}
	if err := config.Validate(cfg); err != nil {
		return report, fmt.Errorf("validate config: %w", err)
	}
	report.Valid = true
	report.ConfigurationValid = true
	report.Pairs = len(cfg.Pairs)
	report.mappings = cfg.Pairs
	for _, source := range cfg.Sources {
		report.Sources = append(report.Sources, validationResource{Name: source.Name, Type: source.Type, Status: "not_tested", host: source.Host, port: source.Port})
	}
	for _, target := range cfg.Targets {
		report.Targets = append(report.Targets, validationResource{Name: target.Name, Type: target.Type, Status: "not_tested"})
	}
	if dryRun {
		return report, nil
	}
	report.ConnectivityTested = true
	registry := builtin.Registry()
	ctx := context.Background()
	if len(contexts) > 0 && contexts[0] != nil {
		ctx = contexts[0]
	}
	var failures []error
	record := func(resource *validationResource, kind string, failure error) {
		if failure == nil {
			resource.Status = "ok"
			return
		}
		resource.Status = "failed"
		resource.Error = failure.Error()
		wrapped := fmt.Errorf("%s %q: %w", kind, resource.Name, failure)
		report.Errors = append(report.Errors, wrapped.Error())
		failures = append(failures, wrapped)
	}
	for i, sourceConfig := range cfg.Sources {
		values, failure := config.SourcePluginConfig(sourceConfig)
		if failure != nil {
			record(&report.Sources[i], "source", failure)
			continue
		}
		source, failure := registry.CreateSource(sourceConfig.Type, values)
		if failure == nil {
			failure = errors.Join(source.Connect(ctx), source.Close())
		}
		record(&report.Sources[i], "source", failure)
	}
	for i, targetConfig := range cfg.Targets {
		values := make(map[string]interface{}, len(targetConfig.Config)+1)
		for key, value := range targetConfig.Config {
			values[key] = value
		}
		values["name"] = targetConfig.Name
		target, failure := registry.CreateTarget(targetConfig.Type, values)
		if failure == nil {
			failure = errors.Join(target.Initialize(ctx), target.Close())
		}
		record(&report.Targets[i], "target", failure)
	}
	return report, errors.Join(failures...)
}

func printValidationReport(report validationReport) error {
	if jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	if report.ConfigurationValid {
		fmt.Println("Configuration is valid")
	} else {
		fmt.Println("Configuration is invalid")
	}
	fmt.Printf("Sources: %d\n", len(report.Sources))
	for _, source := range report.Sources {
		fmt.Printf("  - %s (%s://%s:%d)\n", source.Name, source.Type, source.host, source.port)
	}
	fmt.Printf("Targets: %d\n", len(report.Targets))
	for _, target := range report.Targets {
		fmt.Printf("  - %s (%s)\n", target.Name, target.Type)
	}
	fmt.Printf("Pairs: %d\n", report.Pairs)
	for _, pair := range report.mappings {
		fmt.Printf("  - %s -> %s\n", pair.Source, strings.Join(pair.Targets, ", "))
	}
	if report.ConnectivityTested {
		fmt.Println("\nTesting connectivity...")
		for _, group := range []struct {
			kind      string
			resources []validationResource
		}{{"Source", report.Sources}, {"Target", report.Targets}} {
			for _, resource := range group.resources {
				if resource.Error != "" {
					fmt.Printf("  %s %s: FAILED - %s\n", group.kind, resource.Name, resource.Error)
				} else {
					fmt.Printf("  %s %s: OK\n", group.kind, resource.Name)
				}
			}
		}
	} else {
		for _, message := range report.Errors {
			fmt.Printf("  ERROR: %s\n", message)
		}
	}
	return nil
}
