package sttcorpus

import (
	"bytes"
	_ "embed"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ToolVersion is the variant maker's version (cmd/items-corpus-stt): bump
// it when the pipeline changes what a variant would hold (the layout, the
// voices' handling, the mapping). Tool is a variant's `tool` value; the
// corpus lint refuses a variant made by any other.
const (
	ToolVersion = "1"
	Tool        = "items-corpus-stt " + ToolVersion
)

// VoicesYAML is voices.yaml: the Piper voices the corpus is spoken with.
//
//go:embed voices.yaml
var VoicesYAML []byte

// Voices is the parsed VoicesYAML.
func Voices() (*Config, error) { return ParseConfig(VoicesYAML) }

// Config is voices.yaml.
type Config struct {
	Revision string            `yaml:"revision"`
	Models   map[string]Model  `yaml:"models"`
	Speakers map[string]string `yaml:"speakers"`
}

// Model is one Piper voice model at the pinned revision.
type Model struct {
	Path         string `yaml:"path"`
	Accent       string `yaml:"accent"`
	SHA256       string `yaml:"sha256"`
	ConfigSHA256 string `yaml:"config_sha256"`
}

// Voice is a model and a speaker of it.
type Voice struct {
	Model   string
	Speaker int
}

func (v Voice) String() string { return v.Model + "#" + strconv.Itoa(v.Speaker) }

// ParseConfig reads voices.yaml, refusing unknown fields and a speaker whose
// voice names no model.
func ParseConfig(b []byte) (*Config, error) {
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	c := &Config{}
	if err := d.Decode(c); err != nil {
		return nil, err
	}
	for name := range c.Speakers {
		if _, err := c.voice(name); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *Config) voice(name string) (Voice, error) {
	raw, ok := c.Speakers[name]
	if !ok {
		return Voice{}, fmt.Errorf("speaker %q has no voice in the config", name)
	}
	model, id, ok := strings.Cut(raw, "#")
	n, err := strconv.Atoi(id)
	if !ok || err != nil || n < 0 {
		return Voice{}, fmt.Errorf("speaker %q: voice %q is not <model>#<speaker id>", name, raw)
	}
	if _, ok := c.Models[model]; !ok {
		return Voice{}, fmt.Errorf("speaker %q: no model %q", name, model)
	}
	return Voice{model, n}, nil
}

// VoicesFor is each of a meeting's speakers' voices; no two may share one.
func (c *Config) VoicesFor(speakers []string) (map[string]Voice, error) {
	out := map[string]Voice{}
	by := map[Voice]string{}
	for _, s := range speakers {
		v, err := c.voice(s)
		if err != nil {
			return nil, err
		}
		if o, dup := by[v]; dup {
			return nil, fmt.Errorf("speakers %q and %q share voice %s in one meeting", o, s, v)
		}
		by[v] = s
		out[s] = v
	}
	return out, nil
}

// Accents is the accents the config's models cover.
func (c *Config) Accents() []string {
	set := map[string]bool{}
	for _, m := range c.Models {
		set[m.Accent] = true
	}
	var out []string
	for a := range set {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}
