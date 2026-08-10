// Package notice renders user-visible moderation copy from an embedded,
// per-language catalog. Policy packages emit notice keys; only this package
// holds the text a chat member actually reads.
package notice

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/sompasauna/linkkilinko/pkg/core/moderation"
	"gopkg.in/yaml.v3"
)

//go:embed catalog/*.yaml
var catalogFS embed.FS

const catalogDirectory = "catalog"

// Catalog holds the validated messages for one language.
type Catalog struct {
	language string
	messages map[string]string
}

// Languages lists the catalog languages compiled into the binary.
func Languages() ([]string, error) {
	entries, err := catalogFS.ReadDir(catalogDirectory)
	if err != nil {
		return nil, fmt.Errorf("notice: read catalog directory: %w", err)
	}
	languages := make([]string, 0, len(entries))
	for _, entry := range entries {
		languages = append(languages, strings.TrimSuffix(entry.Name(), ".yaml"))
	}
	sort.Strings(languages)
	return languages, nil
}

// Load reads the embedded catalog for language and verifies that it defines
// exactly the notice keys a moderation plan can emit. An unknown language or
// an incomplete catalog is a startup error, never a runtime surprise.
func Load(language string) (Catalog, error) {
	return load(language, "")
}

// LoadWithOverride loads an embedded catalog and overlays operator-provided text.
func LoadWithOverride(language, overridePath string) (Catalog, error) {
	return load(language, overridePath)
}

func load(language, overridePath string) (Catalog, error) {
	name := strings.ToLower(strings.TrimSpace(language))
	if name == "" {
		return Catalog{}, errors.New("notice: language is empty")
	}
	data, err := catalogFS.ReadFile(path.Join(catalogDirectory, name+".yaml"))
	if err != nil {
		available, listErr := Languages()
		if listErr != nil {
			return Catalog{}, listErr
		}
		return Catalog{}, fmt.Errorf("notice: unsupported language %q (available: %s)", name, strings.Join(available, ", "))
	}
	var messages map[string]string
	if err := yaml.Unmarshal(data, &messages); err != nil {
		return Catalog{}, fmt.Errorf("notice: parse catalog %q: %w", name, err)
	}
	if strings.TrimSpace(overridePath) != "" {
		overrideData, readErr := os.ReadFile(overridePath)
		if readErr != nil {
			return Catalog{}, fmt.Errorf("notice: read override %q: %w", overridePath, readErr)
		}
		var overrides map[string]string
		if err := yaml.Unmarshal(overrideData, &overrides); err != nil {
			return Catalog{}, fmt.Errorf("notice: parse override %q: %w", overridePath, err)
		}
		for key, value := range overrides {
			if strings.TrimSpace(value) == "" {
				return Catalog{}, fmt.Errorf("notice: override %q has empty key %q", overridePath, key)
			}
			messages[key] = value
		}
	}
	if err := validate(name, messages); err != nil {
		return Catalog{}, err
	}
	return Catalog{language: name, messages: messages}, nil
}

// Language returns the catalog's language tag.
func (c Catalog) Language() string { return c.language }

// Render resolves key and substitutes {name} placeholders with params. Values
// are inserted literally in a single pass, so user-controlled text can never
// introduce another placeholder or Telegram markup.
func (c Catalog) Render(key string, params map[string]string) string {
	message, ok := c.messages[key]
	if !ok || len(params) == 0 {
		return message
	}
	pairs := make([]string, 0, len(params)*2)
	for name, value := range params {
		pairs = append(pairs, "{"+name+"}", value)
	}
	return strings.NewReplacer(pairs...).Replace(message)
}

func validate(language string, messages map[string]string) error {
	required := moderation.NoticeKeys()
	known := make(map[string]struct{}, len(required))
	for _, key := range required {
		known[key] = struct{}{}
		if strings.TrimSpace(messages[key]) == "" {
			return fmt.Errorf("notice: catalog %q is missing key %q", language, key)
		}
	}
	unknown := make([]string, 0)
	for key := range messages {
		if _, ok := known[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("notice: catalog %q defines unknown keys: %s", language, strings.Join(unknown, ", "))
	}
	allowed := map[string]map[string]struct{}{
		moderation.NoticeNewcomerSandbox: {},
		moderation.NoticeGoogleWrapper:   {"sender": {}, "content": {}},
		moderation.NoticePreviewMissing:  {},
		moderation.NoticePreviewEnriched: {"sender": {}, "url": {}, "metadata": {}},
	}
	placeholderPattern := regexp.MustCompile(`\{([a-z]+)\}`)
	for key, message := range messages {
		for _, match := range placeholderPattern.FindAllStringSubmatch(message, -1) {
			if _, ok := allowed[key][match[1]]; !ok {
				return fmt.Errorf("notice: catalog %q key %q uses unsupported placeholder {%s}", language, key, match[1])
			}
		}
	}
	return nil
}
