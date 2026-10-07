package hive

import (
	"strings"
	"testing"
)

// validYAML is the C10 example: every field, nothing derived.
const validYAML = `project: hrmny
repo: ssh://git@code.example.com/example/hrmny.git
base_image: elixir-release
gates:
  - mix format --check-formatted
  - mix test
needs: [leases, registry]
max_workers: 3
models: {implement: Z.ai/glm-5.3, verify: Z.ai/glm-5.3}
`

// validProject is what validYAML parses to.
func validProject() Project {
	return Project{
		Project:    "hrmny",
		Repo:       "ssh://git@code.example.com/example/hrmny.git",
		BaseImage:  "elixir-release",
		Gates:      []string{"mix format --check-formatted", "mix test"},
		Needs:      []string{"leases", "registry"},
		MaxWorkers: 3,
		Models: Models{
			Implement: "Z.ai/glm-5.3",
			Verify:    "Z.ai/glm-5.3",
		},
	}
}

func TestParseValid(t *testing.T) {
	p, err := Parse([]byte(validYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := validProject()
	if p.Project != want.Project || p.Repo != want.Repo || p.BaseImage != want.BaseImage ||
		p.MaxWorkers != want.MaxWorkers || p.Models != want.Models {
		t.Fatalf("parsed %+v, want %+v", p, want)
	}
	if strings.Join(p.Gates, "|") != strings.Join(want.Gates, "|") {
		t.Fatalf("gates %v, want %v", p.Gates, want.Gates)
	}
	if strings.Join(p.Needs, "|") != strings.Join(want.Needs, "|") {
		t.Fatalf("needs %v, want %v", p.Needs, want.Needs)
	}
	if problems := p.Validate(); len(problems) != 0 {
		t.Fatalf("valid file has problems: %v", problems)
	}
}

func TestParseDefaults(t *testing.T) {
	p, err := Parse([]byte(`project: alpha
repo: https://code.example.com/example/alpha.git
base_image: go-base
gates: [go test ./...]
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.MaxWorkers != DefaultMaxWorkers {
		t.Errorf("max_workers %d, want default %d", p.MaxWorkers, DefaultMaxWorkers)
	}
	if p.Models.Implement != DefaultModel {
		t.Errorf("models.implement %q, want default %q", p.Models.Implement, DefaultModel)
	}
	if p.Models.Verify != DefaultModel {
		t.Errorf("models.verify %q, want default %q", p.Models.Verify, DefaultModel)
	}
	if p.Needs != nil && len(p.Needs) != 0 {
		t.Errorf("needs %v, want none", p.Needs)
	}
}

func TestParseModelsPartialDefault(t *testing.T) {
	p, err := Parse([]byte(`project: alpha
repo: https://code.example.com/example/alpha.git
base_image: go-base
gates: [go test ./...]
models: {implement: Z.ai/glm-5.3-mini}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Models.Implement != "Z.ai/glm-5.3-mini" {
		t.Errorf("models.implement %q, want Z.ai/glm-5.3-mini", p.Models.Implement)
	}
	if p.Models.Verify != DefaultModel {
		t.Errorf("models.verify %q, want default %q", p.Models.Verify, DefaultModel)
	}
}

func TestParseStrictUnknownField(t *testing.T) {
	for name, doc := range map[string]string{
		"top level": `project: alpha
repo: https://code.example.com/example/alpha.git
base_image: go-base
gates: [go test ./...]
gates_extra: true
`,
		"under models": `project: alpha
repo: https://code.example.com/example/alpha.git
base_image: go-base
gates: [go test ./...]
models: {implement: Z.ai/glm-5.3, reasoning: high}
`,
		"misspelt max_workers": `project: alpha
repo: https://code.example.com/example/alpha.git
base_image: go-base
gates: [go test ./...]
maxworkers: 3
`,
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: unknown field accepted", name)
		}
	}
}

func TestParseStrictWrongType(t *testing.T) {
	if _, err := Parse([]byte("project: alpha\nrepo: https://h/git.git\nbase_image: go-base\ngates: [x]\nmax_workers: many\n")); err == nil {
		t.Error("max_workers: string accepted")
	}
}

func TestParseEmpty(t *testing.T) {
	if _, err := Parse(nil); err == nil {
		t.Error("empty file accepted")
	}
	if _, err := Parse([]byte("\n# only a comment\n")); err == nil {
		t.Error("comment-only file accepted")
	}
}

func TestParseDuplicateField(t *testing.T) {
	doc := strings.Replace(validYAML, "max_workers: 3", "max_workers: 3\nmax_workers: 4", 1)
	if _, err := Parse([]byte(doc)); err == nil {
		t.Error("duplicate field accepted")
	}
}

func TestParseFile(t *testing.T) {
	p := t.TempDir() + "/hive.yaml"
	if err := writeFile(p, validYAML); err != nil {
		t.Fatal(err)
	}
	got, err := ParseFile(p)
	if err != nil {
		t.Fatalf("parse file: %v", err)
	}
	if got.Project != "hrmny" {
		t.Fatalf("project %q", got.Project)
	}
	if _, err := ParseFile(t.TempDir() + "/missing.yaml"); err == nil {
		t.Error("missing file accepted")
	}
}

func TestValidateRules(t *testing.T) {
	valid := validProject()
	cases := []struct {
		name   string
		mutate func(*Project)
		field  string
	}{
		{
			name:   "project uppercase",
			mutate: func(p *Project) { p.Project = "Hrmny" },
			field:  "project",
		},
		{
			name:   "project leading digit",
			mutate: func(p *Project) { p.Project = "1hrmny" },
			field:  "project",
		},
		{
			name:   "project one character",
			mutate: func(p *Project) { p.Project = "h" },
			field:  "project",
		},
		{
			name:   "project underscore",
			mutate: func(p *Project) { p.Project = "two_words" },
			field:  "project",
		},
		{
			name:   "project too long",
			mutate: func(p *Project) { p.Project = strings.Repeat("a", 32) },
			field:  "project",
		},
		{
			name:   "project empty",
			mutate: func(p *Project) { p.Project = "" },
			field:  "project",
		},
		{
			name:   "repo http",
			mutate: func(p *Project) { p.Repo = "http://code.example.com/example/hrmny.git" },
			field:  "repo",
		},
		{
			name:   "repo git scheme",
			mutate: func(p *Project) { p.Repo = "git@code.example.com:example/hrmny.git" },
			field:  "repo",
		},
		{
			name:   "repo bare path",
			mutate: func(p *Project) { p.Repo = "../hrmny" },
			field:  "repo",
		},
		{
			name:   "repo empty",
			mutate: func(p *Project) { p.Repo = "" },
			field:  "repo",
		},
		{
			name:   "base_image empty",
			mutate: func(p *Project) { p.BaseImage = "" },
			field:  "base_image",
		},
		{
			name:   "gates empty",
			mutate: func(p *Project) { p.Gates = nil },
			field:  "gates",
		},
		{
			name:   "unknown needs key",
			mutate: func(p *Project) { p.Needs = []string{"llm"} },
			field:  "needs",
		},
		{
			name:   "needs raw address",
			mutate: func(p *Project) { p.Needs = []string{"10.0.0.1:22"} },
			field:  "needs",
		},
		{
			name:   "max_workers zero",
			mutate: func(p *Project) { p.MaxWorkers = 0 },
			field:  "max_workers",
		},
		{
			name:   "max_workers negative",
			mutate: func(p *Project) { p.MaxWorkers = -1 },
			field:  "max_workers",
		},
		{
			name:   "max_workers too high",
			mutate: func(p *Project) { p.MaxWorkers = 9 },
			field:  "max_workers",
		},
	}
	for _, tc := range cases {
		p := valid
		tc.mutate(&p)
		problems := p.Validate()
		if len(problems) != 1 {
			t.Errorf("%s: got %d problems (%v), want 1", tc.name, len(problems), problems)
			continue
		}
		if problems[0].Field != tc.field {
			t.Errorf("%s: problem field %q, want %q", tc.name, problems[0].Field, tc.field)
		}
		if !strings.HasSuffix(problems[0].Remedy, ".") || !isOneSentence(problems[0].Remedy) {
			t.Errorf("%s: remedy %q is not one sentence", tc.name, problems[0].Remedy)
		}
	}
}

// TestValidateBoundsAndKnowns pins the exact edges of the rules that
// mutate-based cases cannot show on their own.
func TestValidateBoundsAndKnowns(t *testing.T) {
	if got := len("a" + strings.Repeat("b", 30)); got != 31 {
		t.Fatalf("test premise broken: %d", got)
	}

	valid := validProject()
	// 31 characters: one letter plus thirty more, the longest name.
	long := valid
	long.Project = "a" + strings.Repeat("b", 30)
	if problems := long.Validate(); len(problems) != 0 {
		t.Errorf("31-character name rejected: %v", problems)
	}
	two := valid
	two.Project = "ab"
	if problems := two.Validate(); len(problems) != 0 {
		t.Errorf("2-character name rejected: %v", problems)
	}

	for _, n := range []int{1, 8} {
		p := valid
		p.MaxWorkers = n
		if problems := p.Validate(); len(problems) != 0 {
			t.Errorf("max_workers %d rejected: %v", n, problems)
		}
	}

	for _, repo := range []string{
		"ssh://git@code.example.com/example/hrmny.git",
		"https://code.example.com/example/hrmny.git",
		"ssh://git@code.example.com:2222/example/hrmny.git",
	} {
		p := valid
		p.Repo = repo
		if problems := p.Validate(); len(problems) != 0 {
			t.Errorf("repo %q rejected: %v", repo, problems)
		}
	}

	// A repo with no host is not a repo.
	p := valid
	p.Repo = "ssh:///dev/null"
	if problems := p.Validate(); len(problems) != 1 || problems[0].Field != "repo" {
		t.Errorf("host-less repo: got %v", problems)
	}
}

// TestValidateBothBoundsRejected shows the rules compose: several broken
// fields yield several problems, each naming its field.
func TestValidateBothBoundsRejected(t *testing.T) {
	p := Project{Project: "BAD", Repo: "nope", MaxWorkers: 42}
	problems := p.Validate()
	if len(problems) != 5 {
		t.Fatalf("got %d problems, want 5: %v", len(problems), problems)
	}
	fields := map[string]bool{}
	for _, pr := range problems {
		fields[pr.Field] = true
	}
	for _, want := range []string{"project", "repo", "base_image", "gates", "max_workers"} {
		if !fields[want] {
			t.Errorf("no problem for %s", want)
		}
	}
}

// isOneSentence reports whether s reads as one sentence: it ends with a
// full stop and never starts a new sentence after a period (URLs and
// scheme names contain dots, but never ". " followed by a capital).
func isOneSentence(s string) bool {
	if s == "" || !strings.HasSuffix(s, ".") {
		return false
	}
	for i := 0; i+2 < len(s); i++ {
		if s[i] == '.' && s[i+1] == ' ' && s[i+2] >= 'A' && s[i+2] <= 'Z' {
			return false
		}
	}
	return true
}

func TestWorkerImage(t *testing.T) {
	cases := []struct{ base, want string }{
		{"elixir-release", "elixir-release-worker"},
		{"go-base", "go-base-worker"},
		{"dev", "dev-worker"},
	}
	for _, tc := range cases {
		p := Project{BaseImage: tc.base}
		if got := p.WorkerImage(); got != tc.want {
			t.Errorf("WorkerImage(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}

// testInstance is the instance facts the derivation tests use.
func testInstance() Instance {
	return Instance{
		LeaseAPI:     "spoond.example.com:8890",
		Registry:     "spoond.example.com:5000",
		ModelService: "llm.example.com",
		AgentMail:    "mail.example.com",
		Images:       []string{"dev-base", "elixir-release", "go-base"},
	}
}

func TestAllowlistNoNeeds(t *testing.T) {
	p := validProject()
	p.Needs = nil
	inst := testInstance()
	got := p.Allowlist(inst)
	want := []string{"code.example.com", "llm.example.com", "mail.example.com"}
	if !equalStrings(got, want) {
		t.Errorf("Allowlist = %v, want %v", got, want)
	}
}

func TestAllowlistBothNeeds(t *testing.T) {
	p := validProject() // needs: [leases, registry]
	inst := testInstance()
	got := p.Allowlist(inst)
	want := []string{
		"code.example.com",
		"llm.example.com",
		"mail.example.com",
		"spoond.example.com:5000",
		"spoond.example.com:8890",
	}
	if !equalStrings(got, want) {
		t.Errorf("Allowlist = %v, want %v", got, want)
	}
}

func TestAllowlistUnknownNeedsIgnored(t *testing.T) {
	p := validProject()
	p.Needs = []string{"leases", "made-up"}
	inst := testInstance()
	got := p.Allowlist(inst)
	want := []string{"code.example.com", "llm.example.com", "mail.example.com", "spoond.example.com:8890"}
	if !equalStrings(got, want) {
		t.Errorf("Allowlist = %v, want %v", got, want)
	}
}

// TestAllowlistDuplicateAndSorted pins the de-duplication: a repo on the
// same host as the model service, and an empty needs entry list, still
// yield sorted, unique hosts.
func TestAllowlistDuplicateAndSorted(t *testing.T) {
	p := Project{
		Project: "spoond",
		Repo:    "ssh://git@llm.example.com/example/spoond.git",
		Needs:   nil,
	}
	got := p.Allowlist(Instance{
		LeaseAPI:     "spoond.example.com:8890",
		Registry:     "spoond.example.com:8890",
		ModelService: "llm.example.com",
		AgentMail:    "llm.example.com",
	})
	want := []string{"llm.example.com"}
	if !equalStrings(got, want) {
		t.Errorf("Allowlist = %v, want %v (duplicates or unsorted)", got, want)
	}
}

// TestAllowlistPortCarryingTargets shows needs: targets keep their port
// when the fixed target has one.
func TestAllowlistPortCarryingTargets(t *testing.T) {
	p := Project{Project: "alpha", Repo: "https://code.example.com/example/alpha.git", Needs: []string{NeedRegistry}}
	inst := testInstance()
	inst.Registry = "registry.example.com:5000"
	want := []string{"code.example.com", "llm.example.com", "mail.example.com", "registry.example.com:5000"}
	if got := p.Allowlist(inst); !equalStrings(got, want) {
		t.Errorf("Allowlist = %v, want %v", got, want)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
