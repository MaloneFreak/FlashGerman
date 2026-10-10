// Teste da exportação para o Anki: node tools/web/test/anki.test.mjs
import assert from 'node:assert/strict';
import { buildAnkiCsv, buildTsv, cardTags, escapeHtml } from '../../../web/anki.js';

const card = {
    german: 'Er sagt: "Ich hab\' keine Zeit; sorry!" <b>',
    pt: 'Ele diz: "Não tenho tempo; desculpa!"',
    literal: '',
    tense: 'Präsens',
    grammar: 'Linha 1\nLinha 2',
    pronunciation: 'ér zakt',
    context: 'A: Kommst du? B: Nein.',
    themeTag: 'Médico_Saúde',
    level: 'A1-A2',
};

const csv = buildAnkiCsv([card]);
assert.ok(csv.startsWith('﻿#separator:Semicolon\n#html:true\n#tags column:3\n'));

// Parser CSV mínimo (aspas duplas, ; como separador) para conferir a ida e volta.
function parseCsvLine(line) {
    const out = []; let cur = ''; let q = false;
    for (let i = 0; i < line.length; i++) {
        const ch = line[i];
        if (q) {
            if (ch === '"' && line[i + 1] === '"') { cur += '"'; i++; }
            else if (ch === '"') q = false;
            else cur += ch;
        } else if (ch === '"') q = true;
        else if (ch === ';') { out.push(cur); cur = ''; }
        else cur += ch;
    }
    out.push(cur);
    return out;
}
const dataLines = csv.replace('﻿', '').trim().split('\n').filter((l) => !l.startsWith('#'));
assert.equal(dataLines.length, 1, 'uma linha por cartão (sem quebras soltas)');
const fields = parseCsvLine(dataLines[0]);
assert.equal(fields.length, 3);
assert.equal(fields[0], escapeHtml(card.german));
assert.ok(fields[1].includes('Linha 1<br>Linha 2'));
assert.ok(fields[1].includes('style="'), 'HTML do verso preservado');
assert.equal(fields[2], 'FlashGerman FlashGerman::Tema::Médico_Saúde FlashGerman::Tempo::Präsens FlashGerman::Nivel::A1-A2');
assert.equal(cardTags({ themeTag: 'Feira livre aos sábados', tense: 'Konjunktiv II' }), 'FlashGerman FlashGerman::Tema::Feira_livre_aos_sábados FlashGerman::Tempo::Konjunktiv_II');

const tsv = buildTsv([card, card]);
assert.equal(tsv.trim().split('\n').length, 2);
assert.equal(tsv.split('\n')[0].split('\t').length, 3);
console.log('anki.js: todos os testes passaram');
