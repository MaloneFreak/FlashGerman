// Gera web/app.css e copia fontes e ícones para web/vendor (tudo offline, sem CDN).
// Uso: npm install && npm run build
import { execSync } from 'node:child_process';
import { cpSync, mkdirSync, rmSync, writeFileSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const web = join(here, '..', '..', 'web');
const vendor = join(web, 'vendor');
const nm = join(here, 'node_modules');

rmSync(vendor, { recursive: true, force: true });
mkdirSync(join(vendor, 'fonts'), { recursive: true });

// Fontes (latin + latin-ext cobrem português e alemão)
const fonts = [
    ['@fontsource-variable/outfit', 'outfit', 'Outfit Variable', 'normal'],
    ['@fontsource-variable/plus-jakarta-sans', 'plus-jakarta-sans', 'Plus Jakarta Sans Variable', 'normal'],
    ['@fontsource-variable/plus-jakarta-sans', 'plus-jakarta-sans', 'Plus Jakarta Sans Variable', 'italic'],
];
const ranges = {
    latin: 'U+0000-00FF, U+0131, U+0152-0153, U+02BB-02BC, U+02C6, U+02DA, U+02DC, U+0304, U+0308, U+0329, U+2000-206F, U+20AC, U+2122, U+2191, U+2193, U+2212, U+2215, U+FEFF, U+FFFD',
    'latin-ext': 'U+0100-02BA, U+02BD-02C5, U+02C7-02CC, U+02CE-02D7, U+02DD-02FF, U+0304, U+0308, U+0329, U+1D00-1DBF, U+1E00-1E9F, U+1EF2-1EFF, U+2020, U+20A0-20AB, U+20AD-20C0, U+2113, U+2C60-2C7F, U+A720-A7FF',
};
let css = '';
for (const [pkg, prefix, family, style] of fonts) {
    for (const subset of ['latin-ext', 'latin']) {
        const file = `${prefix}-${subset}-wght-${style}.woff2`;
        cpSync(join(nm, pkg, 'files', file), join(vendor, 'fonts', file));
        css += `@font-face{font-family:'${family}';font-style:${style};font-display:swap;font-weight:100 900;src:url(fonts/${file}) format('woff2');unicode-range:${ranges[subset]};}\n`;
    }
}
writeFileSync(join(vendor, 'fonts.css'), css);
for (const [pkg, prefix] of [['@fontsource-variable/outfit', 'outfit'], ['@fontsource-variable/plus-jakarta-sans', 'plus-jakarta-sans']]) {
    cpSync(join(nm, pkg, 'LICENSE'), join(vendor, 'fonts', `${prefix}-LICENSE.txt`));
}

// Font Awesome (apenas o estilo "solid", que é o usado)
const fa = join(nm, '@fortawesome', 'fontawesome-free');
mkdirSync(join(vendor, 'fontawesome', 'css'), { recursive: true });
mkdirSync(join(vendor, 'fontawesome', 'webfonts'), { recursive: true });
for (const f of ['fontawesome.min.css', 'solid.min.css']) cpSync(join(fa, 'css', f), join(vendor, 'fontawesome', 'css', f));
cpSync(join(fa, 'webfonts', 'fa-solid-900.woff2'), join(vendor, 'fontawesome', 'webfonts', 'fa-solid-900.woff2'));
cpSync(join(fa, 'LICENSE.txt'), join(vendor, 'fontawesome', 'LICENSE.txt'));
// O solid.min.css também referencia o .ttf; como todo navegador moderno usa o woff2, removemos a referência.
const solid = join(vendor, 'fontawesome', 'css', 'solid.min.css');
writeFileSync(solid, readFileSync(solid, 'utf8').replace(/,url\(\.\.\/webfonts\/fa-solid-900\.ttf\) format\("truetype"\)/, ''));

// CSS do Tailwind
execSync('npx tailwindcss -c tailwind.config.js -i input.css -o ../../web/app.css --minify', { cwd: here, stdio: 'inherit' });
console.log('Interface pronta em web/.');
