package app

import (
	"context"

	"github.com/AutisticShark/Subtitle-Translator/internal/engine"
)

// Language is a target language (engine.Languages).
type Language = engine.Language

// Translator is the translation engine boundary. The default implementation is
// internal/engine; tests substitute controllable fakes.
type Translator interface {
	// CountCues parses the file without contacting a provider.
	CountCues(source []byte, extension, encoding string) (int, error)
	// Translate produces the translated file for one target language.
	Translate(ctx context.Context, source []byte, extension, target string, config engine.Config,
		progress func(done, total int)) ([]byte, engine.Stats, error)
}

type engineTranslator struct{}

func (engineTranslator) CountCues(source []byte, extension, encoding string) (int, error) {
	return engine.CountCues(source, extension, encoding)
}

func (engineTranslator) Translate(ctx context.Context, source []byte, extension, target string,
	config engine.Config, progress func(done, total int)) ([]byte, engine.Stats, error) {
	return engine.Translate(ctx, source, extension, target, config, progress)
}

// languageOrder lists target language codes in the engine's table order.
func languageOrder() []string { return engine.LanguageOrder }
