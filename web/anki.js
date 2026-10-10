// Funções puras de exportação (sem acesso à página), testáveis com Node.

export function escapeHtml(value) {
    return String(value ?? '')
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#39;');
}

// Texto do usuário → HTML seguro para o Anki, preservando quebras de linha.
function html(value) {
    return escapeHtml(value).replace(/\r?\n/g, '<br>');
}

// Etiquetas do Anki não podem ter espaços; "::" cria hierarquia (FlashGerman::Tema::...).
export function ankiTag(value) {
    return String(value ?? '')
        .normalize('NFC')
        .trim()
        .replace(/\s+/g, '_')
        .replace(/[;"]/g, '')
        .replace(/:{3,}/g, '::');
}

export function cardTags(card) {
    const tags = ['FlashGerman'];
    if (card.themeTag) tags.push(`FlashGerman::Tema::${ankiTag(card.themeTag)}`);
    if (card.tense) tags.push(`FlashGerman::Tempo::${ankiTag(card.tense)}`);
    if (card.level) tags.push(`FlashGerman::Nivel::${ankiTag(card.level)}`);
    return tags.join(' ');
}

// Verso do cartão. As cores funcionam tanto no modo claro quanto no modo noturno do Anki:
// não fixamos a cor do texto principal e os blocos têm fundo claro próprio.
export function buildAnkiBack(card) {
    let back = '<div style="font-family: Arial, sans-serif; font-size: 16px; line-height: 1.45;">';
    back += `<div style="font-weight: bold; font-size: 18px; color: #059669; margin-bottom: 8px;">${html(card.pt)}</div>`;
    if (card.literal) {
        back += `<div style="font-size: 13px; color: #64748b; margin-bottom: 6px;">Literal: <i>${html(card.literal)}</i></div>`;
    }
    if (card.pronunciation) {
        back += `<div style="font-style: italic; color: #64748b; font-size: 13px; margin-bottom: 6px;">🗣️ ${html(card.pronunciation)}</div>`;
    }
    if (card.grammar) {
        back += `<div style="background: #fffbeb; border-left: 3px solid #f59e0b; padding: 6px 10px; font-size: 13px; color: #334155; margin-top: 8px; border-radius: 4px;"><b>💡 Gramática (${html(card.tense)}):</b> ${html(card.grammar)}</div>`;
    }
    if (card.context) {
        back += `<div style="margin-top: 8px; font-size: 13px; color: #334155; background: #f1f5f9; padding: 6px 10px; border-radius: 4px;"><b>💬 Contexto:</b> <i>${html(card.context)}</i></div>`;
    }
    back += '</div>';
    return back;
}

export function buildAnkiFront(card) {
    return html(card.german);
}

// Campo CSV: entre aspas, com aspas internas duplicadas (padrão do Anki e do Excel).
export function csvField(value) {
    return '"' + String(value ?? '').replace(/\r?\n/g, '<br>').replace(/"/g, '""') + '"';
}

// CSV com cabeçalhos do Anki 2.1.54+: o Anki já detecta separador, HTML e etiquetas.
export function buildAnkiCsv(cards) {
    const lines = [
        '#separator:Semicolon',
        '#html:true',
        '#tags column:3',
        '#columns:Frente;Verso;Etiquetas',
    ];
    for (const card of cards) {
        lines.push([buildAnkiFront(card), buildAnkiBack(card), cardTags(card)].map(csvField).join(';'));
    }
    // BOM UTF-8 para acentos (ä, ö, ü, ß, ã, ç) aparecerem certos em qualquer programa.
    return '﻿' + lines.join('\n') + '\n';
}

// TSV para colar em planilhas ou em outros apps (sem quebras de linha nem tabs internas).
export function buildTsv(cards) {
    const clean = (v) => String(v ?? '').replace(/[\t\r\n]+/g, ' ');
    return cards
        .map((c) => [buildAnkiFront(c), buildAnkiBack(c), cardTags(c)].map(clean).join('\t'))
        .join('\n') + '\n';
}
