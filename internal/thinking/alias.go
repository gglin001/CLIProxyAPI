package thinking

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	log "github.com/sirupsen/logrus"
)

// NormalizeLevelAliases trims and validates model-scoped thinking level aliases.
func NormalizeLevelAliases(aliases map[string]string) map[string]string {
	if len(aliases) == 0 {
		return nil
	}
	out := make(map[string]string, len(aliases))
	for rawFrom, rawTo := range aliases {
		from, okFrom := normalizeAliasLevel(rawFrom)
		to, okTo := normalizeAliasLevel(rawTo)
		if !okFrom || !okTo || from == to {
			continue
		}
		out[from] = to
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NormalizeThinkingSupport normalizes thinking capability metadata loaded from config.
func NormalizeThinkingSupport(support *registry.ThinkingSupport) {
	if support == nil {
		return
	}
	support.Aliases = NormalizeLevelAliases(support.Aliases)
}

func normalizeAliasLevel(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", false
	}
	if mode, ok := ParseSpecialSuffix(value); ok {
		switch mode {
		case ModeNone:
			return string(LevelNone), true
		case ModeAuto:
			return string(LevelAuto), true
		}
	}
	if level, ok := ParseLevelSuffix(value); ok {
		return string(level), true
	}
	return "", false
}

func applyModelLevelAlias(config ThinkingConfig, modelInfo *registry.ModelInfo, model string) ThinkingConfig {
	source, ok := aliasSourceFromConfig(config)
	if !ok || modelInfo == nil || modelInfo.Thinking == nil || len(modelInfo.Thinking.Aliases) == 0 {
		return config
	}
	target, ok := modelInfo.Thinking.Aliases[source]
	if !ok {
		return config
	}

	updated, ok := configFromAliasTarget(target)
	if !ok {
		return config
	}
	log.WithFields(log.Fields{
		"model": model,
		"from":  source,
		"to":    target,
	}).Debug("thinking: model level alias applied |")
	return updated
}

func aliasSourceFromConfig(config ThinkingConfig) (string, bool) {
	switch config.Mode {
	case ModeLevel:
		return normalizeAliasLevel(string(config.Level))
	case ModeNone:
		return string(LevelNone), true
	case ModeAuto:
		return string(LevelAuto), true
	default:
		return "", false
	}
}

func configFromAliasTarget(target string) (ThinkingConfig, bool) {
	target, ok := normalizeAliasLevel(target)
	if !ok {
		return ThinkingConfig{}, false
	}
	switch target {
	case string(LevelNone):
		return ThinkingConfig{Mode: ModeNone, Budget: 0}, true
	case string(LevelAuto):
		return ThinkingConfig{Mode: ModeAuto, Budget: -1}, true
	default:
		return ThinkingConfig{Mode: ModeLevel, Level: ThinkingLevel(target)}, true
	}
}
