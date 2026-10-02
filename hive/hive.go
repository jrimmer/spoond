// Package hive implements the swarm controller's project schema and its
// enlistment checks (docs/plans/2026-10-02-swarm-controller.md, C6, C10
// and C11).
//
// A project describes itself in its own repository, in
// .spoond/hive.yaml: the project name, its git repo, the base image its
// workers are built on, the gates that define "done", the extra network
// it needs, how many workers may run at once and which models they use.
// Everything else — the worker image, the network allowlist, the
// credentials — is derived by the hive, never written by hand.
//
// This file holds the schema (Project), its validation and the two pure
// derivations of C10. check.go holds the check engine, offline.go the
// environment the CLI runs it in.
package hive

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"
)

// Defaults applied to an otherwise valid hive.yaml (C10).
const (
	// DefaultMaxWorkers is max_workers when the field is absent.
	DefaultMaxWorkers = 2
	// DefaultModel is the Bifrost name used for models.implement and
	// models.verify when either is absent.
	DefaultModel = "Z.ai/glm-5.3"
	// MinMaxWorkers and MaxMaxWorkers bound max_workers.
	MinMaxWorkers = 1
	MaxMaxWorkers = 8
)

// The known needs: keys, each mapping to one fixed network target
// (C10). No raw addresses in the file.
const (
	// NeedLeases reaches the lease API.
	NeedLeases = "leases"
	// NeedRegistry reaches the image registry.
	NeedRegistry = "registry"
)

// KnownNeeds lists every accepted needs: key, from the needs: table in
// schema.go.
var KnownNeeds = []string{NeedLeases, NeedRegistry}

// workerImageSuffix turns a base image name into its worker image name.
const workerImageSuffix = "-worker"

// Models names the models a project's workers use (Bifrost names).
type Models struct {
	Implement string `yaml:"implement" json:"implement"`
	Verify    string `yaml:"verify" json:"verify"`
}

// Project is one .spoond/hive.yaml: the whole project description, and
// nothing derived. Unknown fields are an error, so a typo never
// silently drops a constraint.
type Project struct {
	Project    string   `yaml:"project" json:"project"`
	Repo       string   `yaml:"repo" json:"repo"`
	BaseImage  string   `yaml:"base_image" json:"base_image"`
	Gates      []string `yaml:"gates" json:"gates"`
	Needs      []string `yaml:"needs" json:"needs"`
	MaxWorkers int      `yaml:"max_workers" json:"max_workers"`
	Models     Models   `yaml:"models" json:"models"`
}

// Problem is one validation failure: the field it belongs to and the
// exact remedy, in one sentence.
type Problem struct {
	Field  string `json:"field"`
	Remedy string `json:"remedy"`
}

// String renders the problem as "field: remedy".
func (p Problem) String() string {
	if p.Field == "" {
		return p.Remedy
	}
	return p.Field + ": " + p.Remedy
}

// projectDoc mirrors Project with optional fields, so an absent field is
// distinguishable from a zero one and defaults can be applied. Decoding
// is strict, so any unknown field (here or under models) is an error.
type projectDoc struct {
	Project    *string    `yaml:"project"`
	Repo       *string    `yaml:"repo"`
	BaseImage  *string    `yaml:"base_image"`
	Gates      []string   `yaml:"gates"`
	Needs      []string   `yaml:"needs"`
	MaxWorkers *int       `yaml:"max_workers"`
	Models     *modelsDoc `yaml:"models"`
}

type modelsDoc struct {
	Implement *string `yaml:"implement"`
	Verify    *string `yaml:"verify"`
}

// Parse decodes hive.yaml bytes into a Project, applying the defaults,
// and fails on anything that is not the schema.
func Parse(data []byte) (Project, error) {
	var doc projectDoc
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return Project{}, errors.New("hive.yaml is empty")
		}
		return Project{}, fmt.Errorf("parse hive.yaml: %w", err)
	}
	p := Project{
		Project:    deref(doc.Project, ""),
		Repo:       deref(doc.Repo, ""),
		BaseImage:  deref(doc.BaseImage, ""),
		Gates:      append([]string(nil), doc.Gates...),
		Needs:      append([]string(nil), doc.Needs...),
		MaxWorkers: DefaultMaxWorkers,
		Models: Models{
			Implement: DefaultModel,
			Verify:    DefaultModel,
		},
	}
	if doc.MaxWorkers != nil {
		p.MaxWorkers = *doc.MaxWorkers
	}
	if doc.Models != nil {
		if doc.Models.Implement != nil {
			p.Models.Implement = *doc.Models.Implement
		}
		if doc.Models.Verify != nil {
			p.Models.Verify = *doc.Models.Verify
		}
	}
	return p, nil
}

// ParseFile reads and parses the hive.yaml at path.
func ParseFile(path string) (Project, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Project{}, fmt.Errorf("read hive.yaml: %w", err)
	}
	return Parse(b)
}

// projectNameRE is the shape of a project name: a lowercase letter, then
// 2-31 characters of lowercase letters, digits and hyphens.
var projectNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)

// Validate checks the schema's rules and returns one Problem per
// violation, in field order. An empty result means the file is valid. The
// rules themselves live in the field table (schema.go), which the guide
// renders, so a rule cannot exist without being taught.
func (p Project) Validate() []Problem {
	var problems []Problem
	for _, f := range Fields() {
		if f.check != nil {
			problems = append(problems, f.check(&p)...)
		}
	}
	return problems
}

// Instance is the set of facts about the running instance a project
// enlists into. The derivations below take it as their only input, so
// they stay pure and the guide (C11) can render them from the same
// configuration the server runs.
type Instance struct {
	// LeaseAPI is the lease API as guests reach it, host:port.
	LeaseAPI string `json:"lease_api"`
	// Registry is the image registry host.
	Registry string `json:"registry"`
	// ModelService is the model service host.
	ModelService string `json:"model_service"`
	// AgentMail is the Agent Mail host.
	AgentMail string `json:"agent_mail"`
	// Images lists the image catalog's names.
	Images []string `json:"images"`
}

// WorkerImage returns the name of the worker image derived from the
// project's base image (C10): the base image plus the one fixed worker
// layer. No per-project Dockerfile.
func (p Project) WorkerImage() string {
	return p.BaseImage + workerImageSuffix
}

// Allowlist returns the network allowlist derived from the project and
// the instance (C10): the repo host, the model service and Agent Mail
// always, plus the fixed target of every needs: entry. Hosts only,
// sorted and de-duplicated.
func (p Project) Allowlist(inst Instance) []string {
	var hosts []string
	if host, err := repoHost(p.Repo); err == nil && host != "" {
		hosts = append(hosts, host)
	}
	hosts = append(hosts, inst.ModelService, inst.AgentMail)
	for _, n := range p.Needs {
		if t := NeedTarget(n, inst); t != "" {
			hosts = append(hosts, t)
		}
	}
	return dedupeSorted(hosts)
}

// Budget is the project's bee-hour budget (C9), which only the owner
// sets.
type Budget struct {
	// Set reports whether a cap exists.
	Set bool `json:"set"`
	// HoursPerDay is the cap, in bee-hours per day.
	HoursPerDay int `json:"hours_per_day"`
}

// repoHost returns the host (with port when the URL carries one) of a
// git URL, without userinfo, scheme or path.
func repoHost(repo string) (string, error) {
	if repo == "" {
		return "", errors.New("empty repo")
	}
	u, err := url.Parse(repo)
	if err != nil {
		return "", err
	}
	if u.Scheme != "ssh" && u.Scheme != "https" {
		return "", fmt.Errorf("scheme %q is not ssh or https", u.Scheme)
	}
	if u.Hostname() == "" {
		return "", errors.New("no host")
	}
	return u.Host, nil
}

func isKnownNeed(n string) bool {
	for _, k := range KnownNeeds {
		if n == k {
			return true
		}
	}
	return false
}

// deref returns *s, or def when s is nil.
func deref(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}

// dedupeSorted sorts hosts and drops duplicates.
func dedupeSorted(hosts []string) []string {
	sort.Strings(hosts)
	out := hosts[:0]
	for i, h := range hosts {
		if i == 0 || h != hosts[i-1] {
			out = append(out, h)
		}
	}
	return out
}
