// SPDX-License-Identifier: AGPL-3.0-or-later

// Package driver generates and submits the load a scenario describes.
package driver

import (
	"errors"
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/loadtest/scenario"
)

// exprLetTag is the value the EXPR variant binds in its step's let block.
const exprLetTag = "frame-"

type tmplDoc struct {
	SpecVersion string     `yaml:"specificationVersion"`
	Extensions  []string   `yaml:"extensions,omitempty"`
	Name        string     `yaml:"name"`
	Steps       []tmplStep `yaml:"steps"`
}

type tmplStep struct {
	Name         string        `yaml:"name"`
	Dependencies []tmplDep     `yaml:"dependencies,omitempty"`
	HostReqs     *tmplHostReqs `yaml:"hostRequirements,omitempty"`
	Space        tmplSpace     `yaml:"parameterSpace"`
	Script       tmplScript    `yaml:"script"`
}

type tmplDep struct {
	DependsOn string `yaml:"dependsOn"`
}

type tmplHostReqs struct {
	Amounts []tmplAmount `yaml:"amounts"`
}

type tmplAmount struct {
	Name string `yaml:"name"`
	Min  int    `yaml:"min"`
}

type tmplSpace struct {
	Params []tmplParam `yaml:"taskParameterDefinitions"`
}

type tmplParam struct {
	Name   string      `yaml:"name"`
	Type   string      `yaml:"type"`
	Range  string      `yaml:"range"`
	Chunks *tmplChunks `yaml:"chunks,omitempty"`
}

type tmplChunks struct {
	DefaultTaskCount int    `yaml:"defaultTaskCount"`
	RangeConstraint  string `yaml:"rangeConstraint"`
}

type tmplScript struct {
	Let     []string    `yaml:"let,omitempty"`
	Actions tmplActions `yaml:"actions"`
}

type tmplActions struct {
	OnRun tmplRun `yaml:"onRun"`
}

type tmplRun struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args,omitempty"`
}

// BuildTemplate returns the OpenJD YAML for one job of class jc. Step i+1
// depends on step i, and each step expands to exactly jc.TasksPerStep tasks:
// sqi caps a task parameter at 1024 values, so a large step splits into a
// Frame range and a Block range whose product is the task count.
func BuildTemplate(jc scenario.JobClass, jobName string, cmd scenario.Command) ([]byte, error) {
	if cmd.Path == "" {
		return nil, errors.New("driver: command path is empty")
	}
	if jc.Steps < 1 {
		return nil, fmt.Errorf("driver: job class has %d steps, want at least 1", jc.Steps)
	}
	chunk := max(jc.Chunk, 1)
	frames, blocks, ok := scenario.SplitTasks(jc.TasksPerStep, chunk)
	if !ok {
		return nil, fmt.Errorf("driver: %d tasks per step with chunk %d cannot be split under sqi's task parameter cap",
			jc.TasksPerStep, chunk)
	}

	doc := tmplDoc{
		SpecVersion: "jobtemplate-2023-09",
		Extensions:  templateExtensions(jc, chunk),
		Name:        jobName,
	}
	for i := 1; i <= jc.Steps; i++ {
		doc.Steps = append(doc.Steps, buildStep(jc, cmd, i, frames, blocks, chunk))
	}
	return yaml.Marshal(doc)
}

func templateExtensions(jc scenario.JobClass, chunk int) []string {
	var ext []string
	if chunk > 1 {
		ext = append(ext, "TASK_CHUNKING")
	}
	if jc.EXPR {
		ext = append(ext, "EXPR")
	}
	return ext
}

func buildStep(jc scenario.JobClass, cmd scenario.Command, n, frames, blocks, chunk int) tmplStep {
	st := tmplStep{
		Name:     "step-" + strconv.Itoa(n),
		HostReqs: hostRequirements(jc),
		Space:    tmplSpace{Params: []tmplParam{frameParam(frames, chunk)}},
		Script: tmplScript{Actions: tmplActions{OnRun: tmplRun{
			Command: cmd.Path,
			Args:    append([]string(nil), cmd.Args...),
		}}},
	}
	if blocks > 1 {
		st.Space.Params = append(st.Space.Params, tmplParam{
			Name: "Block", Type: "INT", Range: "1-" + strconv.Itoa(blocks),
		})
	}
	if n > 1 {
		st.Dependencies = []tmplDep{{DependsOn: "step-" + strconv.Itoa(n-1)}}
	}
	if jc.EXPR {
		st.Script.Let = []string{"tag = '" + exprLetTag + "'"}
		st.Script.Actions.OnRun.Args = append(st.Script.Actions.OnRun.Args, "{{ tag + string(Task.Param.Frame) }}")
	}
	return st
}

func frameParam(frames, chunk int) tmplParam {
	p := tmplParam{Name: "Frame", Type: "INT", Range: "1-" + strconv.Itoa(frames*chunk)}
	if chunk > 1 {
		p.Type = "CHUNK[INT]"
		p.Chunks = &tmplChunks{DefaultTaskCount: chunk, RangeConstraint: "CONTIGUOUS"}
	}
	return p
}

func hostRequirements(jc scenario.JobClass) *tmplHostReqs {
	var amounts []tmplAmount
	if jc.Cores > 0 {
		amounts = append(amounts, tmplAmount{Name: "amount.worker.vcpu", Min: jc.Cores})
	}
	if jc.UsagePool != "" {
		amounts = append(amounts, tmplAmount{Name: "amount.worker.usagepool." + jc.UsagePool, Min: 1})
	}
	if len(amounts) == 0 {
		return nil
	}
	return &tmplHostReqs{Amounts: amounts}
}
