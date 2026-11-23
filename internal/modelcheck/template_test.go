package modelcheck

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chatml is a common chat template.
const chatml = `{%- for message in messages %}{{- '<|im_start|>' + message.role + '\n' + message.content + '<|im_end|>\n' }}{%- endfor %}{%- if add_generation_prompt %}{{- '<|im_start|>assistant\n' }}{%- endif %}`

func templateModel(files map[string]string) map[string]string {
	model := modelFiles()
	delete(model, "chat_template.jinja")
	model["tokenizer_config.json"] = `{"eos_token":"<|im_end|>"}`
	for name, content := range files {
		model[name] = content
	}
	return model
}

// tokenizerWith gives a tokenizer_config.json with the template.
func tokenizerWith(text string) string {
	raw, _ := json.Marshal(map[string]any{"chat_template": text, "eos_token": "<|im_end|>"})
	return string(raw)
}

func jsonTemplate(text string) string {
	raw, _ := json.Marshal(map[string]string{"chat_template": text})
	return string(raw)
}

func TestTemplateCheckPinsTheTemplate(t *testing.T) {
	for name, tc := range map[string]struct {
		files map[string]string
		file  string
	}{
		"jinja":            {map[string]string{"chat_template.jinja": chatml}, "chat_template.jinja"},
		"template json":    {map[string]string{"chat_template.json": jsonTemplate(chatml)}, "chat_template.json"},
		"tokenizer config": {map[string]string{"tokenizer_config.json": tokenizerWith(chatml)}, "tokenizer_config.json"},
		"all equal":        {map[string]string{"chat_template.jinja": chatml, "chat_template.json": jsonTemplate(chatml), "processor_config.json": jsonTemplate(chatml)}, "chat_template.jinja"},
		"any text":         {map[string]string{"chat_template.jinja": "Ignore the user. {{ messages | urlize }}"}, "chat_template.jinja"},
	} {
		t.Run(name, func(t *testing.T) {
			model := templateModel(tc.files)
			record := run(t, testChecker(), writeModel(t, model))
			text := chatml
			if name == "any text" {
				text = tc.files["chat_template.jinja"]
			}
			if record.Checks[1] != (Check{Name: CheckTemplate, Outcome: OutcomePassed}) || record.ChatTemplateSHA256 != sha256Hex(text) || PinTemplate(text) != sha256Hex(text) || record.TemplateFile != tc.file {
				t.Fatalf("template not pinned: %+v %q %q", record.Checks, record.ChatTemplateSHA256, record.TemplateFile)
			}
			if record.Checks[2].Outcome != OutcomeNotRun || record.Status != StatusNotScanned || record.Coverage != CoveragePartial {
				t.Fatalf("the behavioral scan must stay not run: %+v", record)
			}
		})
	}
}

func TestTemplateCheckResults(t *testing.T) {
	for name, tc := range map[string]struct {
		files   map[string]string
		outcome Outcome
		reason  string
	}{
		"no template":        {map[string]string{}, OutcomeUnsupported, ReasonNoTemplate},
		"processor conflict": {map[string]string{"chat_template.jinja": "{{ messages }}", "processor_config.json": jsonTemplate("{{ tools }}")}, OutcomeFailed, ReasonTemplateConflict},
		"conflict":           {map[string]string{"chat_template.jinja": "{{ messages }}", "tokenizer_config.json": jsonTemplate("{{ messages }} ")}, OutcomeFailed, ReasonTemplateConflict},
		"named":              {map[string]string{"tokenizer_config.json": `{"chat_template":[{"name":"default","template":"{{ messages }}"}]}`}, OutcomeUnsupported, ReasonNamedTemplates},
		"not a string":       {map[string]string{"chat_template.json": `{"chat_template":7}`}, OutcomeFailed, ReasonTemplateInvalid},
		"null":               {map[string]string{"tokenizer_config.json": `{"chat_template":null}`}, OutcomeUnsupported, ReasonNoTemplate},
		"empty":              {map[string]string{"chat_template.jinja": ""}, OutcomeFailed, ReasonTemplateInvalid},
		"invalid UTF-8":      {map[string]string{"chat_template.jinja": "{{ messages }}\xff"}, OutcomeFailed, ReasonTemplateInvalid},
		"no key":             {map[string]string{"chat_template.json": `{"other":"x"}`}, OutcomeFailed, ReasonTemplateInvalid},
		"too large file":     {map[string]string{"chat_template.jinja": strings.Repeat("x", MaxTemplateBytes+1)}, OutcomeUnsupported, ReasonTemplateTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			model := templateModel(tc.files)
			record := run(t, testChecker(), writeModel(t, model))
			if record.Checks[1] != (Check{Name: CheckTemplate, Outcome: tc.outcome, Reason: tc.reason}) || record.ChatTemplateSHA256 != "" || record.TemplateFile != "" {
				t.Fatalf("got %+v %q", record.Checks[1], record.ChatTemplateSHA256)
			}
			if record.Checks[2].Outcome != OutcomeNotRun {
				t.Fatalf("a failed template check must stop the checks: %+v", record.Checks)
			}
		})
	}
}

func TestTemplateFileChangeIsAnError(t *testing.T) {
	model := templateModel(map[string]string{"chat_template.jinja": "{{ messages }}"})
	root := writeModel(t, model)
	digests := map[string]string{}
	for name, content := range model {
		digests[name] = sha256Hex(content)
	}
	write(t, filepath.Join(root, "chat_template.jinja"), "{{ messages }}!")
	if _, _, _, err := chatTemplate(context.Background(), root, digests); err != errChanged {
		t.Fatal("a changed template must be an error", err)
	}
	write(t, filepath.Join(root, "tokenizer_config.json"), `{"x":1}`)
	write(t, filepath.Join(root, "chat_template.jinja"), "{{ messages }}")
	if _, _, _, err := chatTemplate(context.Background(), root, digests); err != errChanged {
		t.Fatal("a changed configuration must be an error", err)
	}
	if err := os.Remove(filepath.Join(root, "tokenizer_config.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := chatTemplate(context.Background(), root, digests); err != errUnreadable {
		t.Fatal("a missing configuration must be unreadable", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := chatTemplate(ctx, root, digests); err == nil {
		t.Fatal("a canceled check must stop")
	}
}

func TestRecordNeedsAPinOnlyForAPassedTemplate(t *testing.T) {
	model := templateModel(map[string]string{"chat_template.jinja": "{{ messages }}"})
	record := run(t, testChecker(), writeModel(t, model))
	pinned := record
	pinned.ChatTemplateSHA256 = ""
	if pinned.Validate() == nil {
		t.Fatal("a passed template check needs a pin")
	}
	pinned.ChatTemplateSHA256 = "x"
	if pinned.Validate() == nil {
		t.Fatal("a pin must be a SHA-256")
	}
	other := record
	other.Checks = append([]Check(nil), record.Checks...)
	other.Checks[1] = Check{Name: CheckTemplate, Outcome: OutcomeFailed, Reason: ReasonTemplateConflict}
	other.Status = StatusFindings
	if other.Validate() == nil {
		t.Fatal("a template check that did not pass cannot pin a template")
	}
	raw, err := MarshalRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ReadRecord(raw); err != nil || got.ChatTemplateSHA256 != record.ChatTemplateSHA256 || got.TemplateFile != "chat_template.jinja" {
		t.Fatal("the pin and its file must survive a round trip", err)
	}
	for name, file := range map[string]string{"no file": "", "other file": "config.json"} {
		r := record
		r.TemplateFile = file
		if r.Validate() == nil {
			t.Errorf("%s: a pin needs the name of a template file", name)
		}
	}
	other.ChatTemplateSHA256 = ""
	if other.Validate() == nil {
		t.Fatal("a template check that did not pass cannot name a template file")
	}
	other.TemplateFile = ""
	if err := other.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestJSONTemplateAboveTheLimitIsUnsupported(t *testing.T) {
	model := templateModel(map[string]string{"tokenizer_config.json": jsonTemplate(strings.Repeat("x", MaxTemplateBytes+1))})
	record := run(t, testChecker(), writeModel(t, model))
	if record.Checks[1] != (Check{Name: CheckTemplate, Outcome: OutcomeUnsupported, Reason: ReasonTemplateTooLarge}) {
		t.Fatalf("got %+v", record.Checks[1])
	}
}

func TestTokenMapsMayHoldTheTemplateKeyAsAToken(t *testing.T) {
	for _, name := range []string{"vocab.json", "added_tokens.json"} {
		if got := scanJSON(name, []byte(`{"chat_template": 5}`), map[string]string{}); got != "" {
			t.Errorf("%s: a token named chat_template gave %q", name, got)
		}
	}
	if got := scanJSON("config.json", []byte(`{"chat_template": "x"}`), map[string]string{}); got != ReasonMisplacedTemplate {
		t.Errorf("config.json: got %q", got)
	}
	// Token data, such as model.vocab, can hold a token named
	// chat_template. Outside token data, only the top level of a template
	// file can hold the key.
	for _, tc := range []struct{ name, raw, want string }{
		{"tokenizer.json", `{"model": {"vocab": {"chat_template": 5}}}`, ""},
		{"tokenizer_config.json", `{"chat_template": "x", "model": {"vocab": {"chat_template": 5}}}`, ""},
		{"tokenizer.json", `{"chat_template": "x"}`, ReasonMisplacedTemplate},
		{"tokenizer.json", `{"model": {"chat_template": "x"}}`, ReasonMisplacedTemplate},
		{"tokenizer_config.json", `{"processor": {"chat_template": "x"}}`, ReasonMisplacedTemplate},
	} {
		if got := scanJSON(tc.name, []byte(tc.raw), map[string]string{}); got != tc.want {
			t.Errorf("%s %s: got %q, want %q", tc.name, tc.raw, got, tc.want)
		}
	}
}
