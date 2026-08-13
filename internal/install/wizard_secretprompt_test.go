package install

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/run-llama/llamacloudctl/internal/tui"
	"github.com/run-llama/llamacloudctl/internal/values"
)

// This file is the anti-leak gate for the wizard's prompt surface.
//
// A credential prompt asked through Input echoes the typed value into the
// terminal, the scrollback and any tee'd install log — that is how a storage
// account key ended up in a live install transcript. The rule that replaced it
// is objective rather than a curated list: a prompt whose answer is written to a
// values path the redaction vocabulary treats as secret (values.IsSecretPath —
// the SAME matcher that masks the rendered-values echo) must be asked through
// the masked Secret method.
//
// The audit below enforces exactly that, end-to-end and without naming prompts:
// every text prompt is answered with a UNIQUE sentinel, the wizard runs, and the
// produced values are walked. Any sentinel that lands on a secret path is traced
// back to the prompt that produced it, which must have been Secret. A new
// credential prompt added through Input therefore fails this test the moment its
// answer reaches a secret key — no maintenance of a title list required.

// maskAuditPrompter answers every text prompt with a unique sentinel and records
// which method produced it, so a value found in the assembled values can be
// traced back to the prompt that asked for it.
//
// Prompts whose default parses as an integer are answered with that default
// instead: those are ports, replica counts and db numbers the wizard parses with
// strconv, and a sentinel there aborts the run before the audit can reach the
// prompts it cares about. None of them is a credential.
type maskAuditPrompter struct {
	n int

	askedBy map[string]string // sentinel -> "Input" | "Secret"
	titleOf map[string]string // sentinel -> prompt title

	selects  map[string]string // prompt title -> forced answer
	multi    []string          // MultiSelect answer
	confirms map[string]bool   // prompt title -> forced answer
}

func newMaskAuditPrompter() *maskAuditPrompter {
	return &maskAuditPrompter{
		askedBy:  map[string]string{},
		titleOf:  map[string]string{},
		selects:  map[string]string{},
		multi:    []string{"openai"},
		confirms: map[string]bool{},
	}
}

func (p *maskAuditPrompter) answer(method, title, def string) string {
	// Integer-valued prompts keep their default (see the type comment).
	if _, err := strconv.Atoi(strings.TrimSpace(def)); err == nil && def != "" {
		return def
	}
	p.n++
	sentinel := fmt.Sprintf("SENTINEL-%03d", p.n)
	p.askedBy[sentinel] = method
	p.titleOf[sentinel] = title
	return sentinel
}

func (p *maskAuditPrompter) Input(title, def string) (string, error) {
	return p.answer("Input", title, def), nil
}

func (p *maskAuditPrompter) Secret(title, def string) (string, error) {
	return p.answer("Secret", title, def), nil
}

func (p *maskAuditPrompter) Confirm(title string, def bool) (bool, error) {
	if a, ok := p.confirms[title]; ok {
		return a, nil
	}
	return def, nil
}

func (p *maskAuditPrompter) Select(title string, _ []string, def string) (string, error) {
	if a, ok := p.selects[title]; ok {
		return a, nil
	}
	return def, nil
}

func (p *maskAuditPrompter) MultiSelect(_ string, _ []string, _ []string) ([]string, error) {
	return p.multi, nil
}

func (p *maskAuditPrompter) Note(string) {}

var _ tui.Prompter = (*maskAuditPrompter)(nil)

// auditRun drives one wizard configuration and returns the prompter (carrying
// the sentinel bookkeeping) plus the assembled values.
func auditRun(t *testing.T, configure func(*maskAuditPrompter), opts WizardOptions) (*maskAuditPrompter, *values.Values) {
	t.Helper()
	p := newMaskAuditPrompter()
	configure(p)
	v := mkValues(t, nil)
	if _, err := RunWizard(p, v, opts); err != nil {
		t.Fatalf("RunWizard: %v", err)
	}
	return p, v
}

// TestWizardPrompts_NoSecretIsAskedInPlaintext is the audit itself. It runs the
// wizard across the configurations that reach the credential prompts (minimal
// AWS, minimal Azure s3proxy, OIDC + static AWS keys, external endpoints, and
// each structured LLM provider) and asserts that no answer reaching a
// secret-redacted values key was collected through the plaintext Input.
func TestWizardPrompts_NoSecretIsAskedInPlaintext(t *testing.T) {
	cases := map[string]struct {
		configure func(*maskAuditPrompter)
		opts      WizardOptions
	}{
		"minimal aws, license JWT + basic auth + openai key": {
			configure: func(p *maskAuditPrompter) {},
		},
		"minimal aws, static object-storage keys": {
			configure: func(p *maskAuditPrompter) {
				p.selects[objectStorageAuthPrompt] = objectStorageAuthStatic
			},
		},
		"minimal azure, s3proxy JCLOUDS credential": {
			configure: func(p *maskAuditPrompter) {},
			opts:      WizardOptions{Cloud: "azure"},
		},
		"license by existing secret name": {
			configure: func(p *maskAuditPrompter) {
				p.selects["License source"] = "reference existing secret"
			},
		},
		"oidc client secret": {
			configure: func(p *maskAuditPrompter) {
				p.selects["Authentication mode"] = "oidc"
			},
		},
		"bedrock static keys": {
			configure: func(p *maskAuditPrompter) {
				p.multi = []string{"bedrock", "openai"}
				p.selects[bedrockAuthPrompt] = bedrockAuthStatic
			},
		},
		"bedrock existing secret name": {
			configure: func(p *maskAuditPrompter) {
				p.multi = []string{"bedrock", "openai"}
				p.selects[bedrockAuthPrompt] = bedrockAuthExistingSecr
			},
		},
		"vertex service-account JSON": {
			configure: func(p *maskAuditPrompter) {
				p.multi = []string{"vertex", "openai"}
			},
		},
		"azure openai deployment key": {
			configure: func(p *maskAuditPrompter) {
				p.multi = []string{"azure_openai", "openai"}
			},
		},
		"external postgres and redis endpoints": {
			configure: func(p *maskAuditPrompter) {
				p.confirms["Customize data services and object storage? (default: install databases in-cluster + use an existing S3 bucket you name)"] = true
				p.selects["postgres: how should it be provided?"] = string(modeExisting)
				p.selects["redis: how should it be provided?"] = string(modeExisting)
			},
		},
		"customize path on azure (s3proxy + workload identity)": {
			configure: func(p *maskAuditPrompter) {
				p.confirms["Customize data services and object storage? (default: install databases in-cluster + use an existing S3 bucket you name)"] = true
			},
			opts: WizardOptions{Cloud: "azure"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p, v := auditRun(t, tc.configure, tc.opts)

			leaks := 0
			masked := 0
			for path, leaf := range leafPaths(v.Map()) {
				sentinel, ok := leaf.(string)
				if !ok {
					continue
				}
				method, fromPrompt := p.askedBy[sentinel]
				if !fromPrompt || !values.IsSecretPath(path) {
					continue
				}
				if method != "Secret" {
					leaks++
					t.Errorf("prompt %q is asked with %s but its answer lands on the secret values key %q — it echoes the credential into the terminal and any tee'd log; use Prompter.Secret",
						p.titleOf[sentinel], method, path)
					continue
				}
				masked++
			}
			// A run that collected NO credential at all would pass vacuously, which
			// would silently hollow out this audit.
			if masked == 0 && leaks == 0 {
				t.Errorf("no prompt answer reached a secret values key in this run — the audit proved nothing; check the wizard path still collects a credential")
			}
		})
	}
}

// TestWizardPrompts_KnownCredentialsAreMasked names the credentials the live
// defect and the ticket called out explicitly, so the generic audit above cannot
// pass merely because a prompt stopped being reached. Each entry is a prompt
// title that MUST have been asked through the masked path.
func TestWizardPrompts_KnownCredentialsAreMasked(t *testing.T) {
	cases := []struct {
		name      string
		configure func(*maskAuditPrompter)
		opts      WizardOptions
		want      []string
	}{
		{
			name:      "license JWT, basic-auth jwtSecret and the openai API key",
			configure: func(p *maskAuditPrompter) {},
			want:      []string{"License JWT", "Basic auth JWT secret", "openai API key"},
		},
		{
			name: "the JCLOUDS storage credential that leaked live",
			opts: WizardOptions{Cloud: "azure"},
			configure: func(p *maskAuditPrompter) {
				p.selects["Authentication mode"] = "oidc"
			},
			want: []string{"Storage access key / credential (JCLOUDS_CREDENTIAL)", "OIDC client secret"},
		},
		{
			name: "static AWS object-storage keys",
			configure: func(p *maskAuditPrompter) {
				p.selects[objectStorageAuthPrompt] = objectStorageAuthStatic
			},
			want: []string{"AWS access key ID", "AWS secret access key"},
		},
		{
			name: "bedrock static keys",
			configure: func(p *maskAuditPrompter) {
				p.multi = []string{"bedrock", "openai"}
				p.selects[bedrockAuthPrompt] = bedrockAuthStatic
			},
			want: []string{bedrockAccessKeyPrompt, bedrockSecretKeyPrompt},
		},
		{
			name: "vertex service-account credentials JSON",
			configure: func(p *maskAuditPrompter) {
				p.multi = []string{"vertex", "openai"}
			},
			want: []string{vertexCredsJSONPrompt},
		},
		{
			name: "azure openai deployment API key",
			configure: func(p *maskAuditPrompter) {
				p.multi = []string{"azure_openai", "openai"}
			},
			want: []string{azureOpenAIAPIKeyPrompt},
		},
		{
			name: "external postgres and redis passwords",
			configure: func(p *maskAuditPrompter) {
				p.confirms["Customize data services and object storage? (default: install databases in-cluster + use an existing S3 bucket you name)"] = true
				p.selects["postgres: how should it be provided?"] = string(modeExisting)
				p.selects["redis: how should it be provided?"] = string(modeExisting)
			},
			want: []string{"postgres password", "redis password"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := auditRun(t, tc.configure, tc.opts)
			for _, title := range tc.want {
				method, asked := methodFor(p, title)
				if !asked {
					t.Errorf("prompt %q was never reached; this configuration no longer covers it", title)
					continue
				}
				if method != "Secret" {
					t.Errorf("prompt %q was asked with %s, want the masked Secret path", title, method)
				}
			}
		})
	}
}

// methodFor reports which Prompter method asked a given title, and whether the
// title was asked at all.
func methodFor(p *maskAuditPrompter, title string) (string, bool) {
	for sentinel, got := range p.titleOf {
		if got == title {
			return p.askedBy[sentinel], true
		}
	}
	return "", false
}

// leafPaths flattens a values map into dotted path -> leaf value, descending
// through nested maps and slices the same way the redaction walk does (slice
// elements are addressed by their index).
func leafPaths(m map[string]any) map[string]any {
	out := map[string]any{}
	var walk func(prefix string, val any)
	walk = func(prefix string, val any) {
		switch t := val.(type) {
		case map[string]any:
			for k, child := range t {
				walk(join(prefix, k), child)
			}
		case []any:
			for i, child := range t {
				walk(join(prefix, strconv.Itoa(i)), child)
			}
		default:
			out[prefix] = val
		}
	}
	for k, v := range m {
		walk(k, v)
	}
	return out
}

func join(prefix, seg string) string {
	if prefix == "" {
		return seg
	}
	return prefix + "." + seg
}
