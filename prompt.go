package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// GenerateParams são as opções escolhidas na tela do gerador.
type GenerateParams struct {
	Theme         string `json:"theme"`
	Tense         string `json:"tense"`
	Level         string `json:"level"`
	Count         int    `json:"count"`
	Colloquial    bool   `json:"colloquial"`
	Pronunciation bool   `json:"pronunciation"`
}

// Card é um cartão já normalizado, pronto para a interface.
type Card struct {
	German        string `json:"german"`
	PT            string `json:"pt"`
	Literal       string `json:"literal"`
	Tense         string `json:"tense"`
	Grammar       string `json:"grammar"`
	Pronunciation string `json:"pronunciation"`
	Context       string `json:"context"`
}

var validLevels = map[string]bool{"A1-A2": true, "B1-B2": true, "C1": true}

func (p *GenerateParams) Validate() error {
	p.Theme = strings.TrimSpace(p.Theme)
	if p.Theme == "" {
		return errors.New("escolha ou digite um tema")
	}
	if utf8.RuneCountInString(p.Theme) > 200 {
		return errors.New("o tema pode ter no máximo 200 caracteres")
	}
	if !validLevels[p.Level] {
		p.Level = "A1-A2"
	}
	if _, ok := tenseInstructions[p.Tense]; !ok {
		p.Tense = "mixed"
	}
	if p.Count < 1 {
		p.Count = 1
	}
	if p.Count > 30 {
		p.Count = 30
	}
	return nil
}

var tenseInstructions = map[string]string{
	"praesens":    "Apenas no tempo PRÄSENS (presente).",
	"perfekt":     "Apenas no tempo PERFEKT (passado falado comum).",
	"praeteritum": "Apenas no tempo PRÄTERITUM (passado escrito/narrativo; no dia a dia, principalmente com sein, haben e verbos modais).",
	"futur":       "Apenas no tempo FUTUR I (futuro).",
	"konjunktiv2": "Apenas no modo KONJUNKTIV II (cortesia, desejos, hipóteses).",
	"mixed":       "Misture tempos verbais do dia a dia (Präsens, Perfekt e Konjunktiv II).",
}

const systemPrompt = `Você é um linguista nativo em alemão e professor especializado em ensinar alemão prático e real do dia a dia para brasileiros que moram na Alemanha.
Seu objetivo é gerar frases em alemão de uso diário autêntico, com tradução natural para o português do Brasil (PT-BR).

REGRAS DE QUALIDADE:
1. As frases DEVEM ser expressões reais, faladas no cotidiano (evite traduções ao pé da letra e frases engessadas de livro antigo).
2. Se um tempo verbal específico for pedido (Präsens, Perfekt, Präteritum, Futur I ou Konjunktiv II), TODAS as frases devem usar esse tempo verbal.
3. Perfekt: use o passado falado natural (ex.: "Ich hab' das schon gemacht", "Bist du gut angekommen?").
4. Konjunktiv II: use a linguagem educada de pedidos e hipóteses (ex.: "Ich hätte gerne einen Kaffee", "Könntest du mir kurz helfen?").
5. As explicações gramaticais devem focar nas dificuldades de quem fala português (verbos separáveis, Dativ/Akkusativ, posição do verbo, gênero dos substantivos, preposições).
6. O alemão precisa estar 100% correto (ortografia, declinação, uso de ß, ä, ö, ü). Não repita frases.
7. Tradução, explicação gramatical e dica de pronúncia são escritas em português do Brasil. O mini diálogo de contexto é em alemão.

FORMATO DA RESPOSTA:
Responda SOMENTE com um objeto JSON válido, sem nenhum texto antes ou depois e sem blocos de código, exatamente neste formato:
{"frases":[{"german_phrase":"...","pt_translation":"...","literal_translation":"...","tense":"...","grammar_note":"...","pronunciation_tip":"...","context_dialogue":"..."}]}

Significado de cada campo:
- german_phrase: a frase natural em alemão
- pt_translation: tradução idiomática em PT-BR
- literal_translation: tradução literal palavra por palavra em PT-BR (ou "" se não ajudar)
- tense: tempo verbal principal (Präsens, Perfekt, Präteritum, Futur I ou Konjunktiv II)
- grammar_note: explicação curta (1 ou 2 frases) sobre a estrutura, pensada para brasileiros
- pronunciation_tip: dica de pronúncia para brasileiros (ou "" se não foi pedida)
- context_dialogue: mini diálogo de 2 falas em alemão usando a frase, no formato "A: ... B: ..."`

func buildPrompts(p GenerateParams) (system, user string) {
	colloquial := "NÃO (use o alemão padrão, sem reduções)"
	if p.Colloquial {
		colloquial = "SIM (use reduções e expressões faladas reais, como hab', 's, mal, halt, eben)"
	}
	pron := "NÃO (deixe pronunciation_tip vazio)"
	if p.Pronunciation {
		pron = "SIM"
	}
	levelHint := map[string]string{
		"A1-A2": "frases curtas e vocabulário básico",
		"B1-B2": "frases de tamanho médio, conectivos e vocabulário do cotidiano",
		"C1":    "estruturas complexas, expressões idiomáticas e vocabulário rico",
	}[p.Level]

	user = fmt.Sprintf(`Gere exatamente %d frases em alemão para o tema: "%s".
Nível CEFR: %s (%s).
Tempo verbal solicitado: %s
Linguagem coloquial/falada do dia a dia: %s.
Incluir dica de pronúncia adaptada para brasileiros: %s.
Responda apenas com o JSON no formato pedido, com exatamente %d itens em "frases".`,
		p.Count, p.Theme, p.Level, levelHint, tenseInstructions[p.Tense], colloquial, pron, p.Count)
	return systemPrompt, user
}

var cardFields = []string{"german_phrase", "pt_translation", "literal_translation", "tense", "grammar_note", "pronunciation_tip", "context_dialogue"}

// buildSchema devolve o JSON Schema da resposta. Para o llama.cpp local, fixamos também
// a quantidade de itens: a gramática obriga o modelo a gerar exatamente "count" frases.
func buildSchema(count int, exactCount bool) map[string]any {
	props := map[string]any{}
	for _, f := range cardFields {
		props[f] = map[string]any{"type": "string"}
	}
	items := map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             cardFields,
		"additionalProperties": false,
	}
	arr := map[string]any{"type": "array", "items": items}
	if exactCount {
		arr["minItems"] = count
		arr["maxItems"] = count
	}
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"frases": arr},
		"required":             []string{"frases"},
		"additionalProperties": false,
	}
}

var (
	reThink = regexp.MustCompile(`(?is)<think(?:ing)?>.*?</think(?:ing)?>`)
	reFence = regexp.MustCompile("(?s)```(?:json|JSON)?\\s*(.*?)```")
)

// parseCards extrai os cartões do texto do modelo, tolerando sujeira comum:
// blocos de código, texto antes/depois, raciocínio <think> e respostas cortadas no meio.
func parseCards(text string) ([]Card, error) {
	text = reThink.ReplaceAllString(text, "")
	if m := reFence.FindStringSubmatch(text); m != nil && strings.ContainsAny(m[1], "{[") {
		text = m[1]
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("o modelo devolveu uma resposta vazia")
	}

	start := strings.IndexAny(text, "{[")
	if start < 0 {
		return nil, errors.New("o modelo não devolveu JSON: " + truncate(text, 160))
	}
	raw := text[start:]

	var items []map[string]any
	if raw[0] == '[' {
		items = decodeArrayLenient([]byte(raw))
	} else {
		// Objeto: procura a primeira lista de objetos dentro dele.
		var obj map[string]json.RawMessage
		dec := json.NewDecoder(strings.NewReader(raw))
		if err := dec.Decode(&obj); err == nil {
			for _, key := range []string{"frases", "phrases", "cards", "flashcards", "items", "data"} {
				if v, ok := obj[key]; ok {
					items = decodeArrayLenient(v)
					break
				}
			}
			if items == nil {
				for _, v := range obj {
					if t := bytes.TrimSpace(v); len(t) > 0 && t[0] == '[' {
						items = decodeArrayLenient(t)
						if len(items) > 0 {
							break
						}
					}
				}
			}
			if items == nil {
				// Um único cartão solto.
				var single map[string]any
				if json.Unmarshal([]byte(raw[:dec.InputOffset()]), &single) == nil {
					items = []map[string]any{single}
				}
			}
		} else {
			// JSON cortado: recupera os itens completos depois de "frases": [
			if i := strings.Index(raw, "["); i >= 0 {
				items = decodeArrayLenient([]byte(raw[i:]))
			}
		}
	}

	var cards []Card
	seen := map[string]bool{}
	for _, it := range items {
		c := Card{
			German:        pickString(it, "german_phrase", "german", "frase", "de", "phrase", "deutsch"),
			PT:            pickString(it, "pt_translation", "translation", "traducao", "tradução", "pt", "portuguese"),
			Literal:       pickString(it, "literal_translation", "literal"),
			Tense:         pickString(it, "tense", "tempo", "zeitform"),
			Grammar:       pickString(it, "grammar_note", "grammar", "gramatica", "gramática", "explanation"),
			Pronunciation: pickString(it, "pronunciation_tip", "pronunciation", "pronuncia", "pronúncia"),
			Context:       pickString(it, "context_dialogue", "context", "dialogue", "dialogo", "diálogo"),
		}
		if c.German == "" || c.PT == "" {
			continue
		}
		key := strings.ToLower(c.German)
		if seen[key] {
			continue
		}
		seen[key] = true
		if c.Tense == "" {
			c.Tense = "Geral"
		}
		cards = append(cards, c)
	}
	if len(cards) == 0 {
		return nil, errors.New("não encontrei frases válidas na resposta do modelo: " + truncate(text, 160))
	}
	return cards, nil
}

// decodeArrayLenient lê os elementos de uma lista JSON um a um e para no primeiro erro,
// mantendo o que já foi lido (útil quando a resposta foi cortada por limite de tokens).
func decodeArrayLenient(data []byte) []map[string]any {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil
	}
	var out []map[string]any
	for dec.More() {
		var v map[string]any
		if err := dec.Decode(&v); err != nil {
			break
		}
		out = append(out, v)
	}
	return out
}

func pickString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case string:
				return strings.TrimSpace(t)
			case []any:
				var parts []string
				for _, x := range t {
					if s, ok := x.(string); ok {
						parts = append(parts, strings.TrimSpace(s))
					}
				}
				return strings.Join(parts, " ")
			}
		}
	}
	return ""
}
