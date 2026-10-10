import { escapeHtml, buildAnkiCsv, buildTsv } from './anki.js';

// ---------------------------------------------------------------------------
// Básico

const TOKEN = document.querySelector('meta[name="fg-token"]')?.content || '';
const VERSION = document.querySelector('meta[name="fg-version"]')?.content || '';
const CLIENT_ID = (crypto.randomUUID ? crypto.randomUUID() : String(Math.random()).slice(2));
const $ = (id) => document.getElementById(id);

async function api(path, { method = 'GET', body } = {}) {
    const opts = { method, headers: { 'X-FG-Token': TOKEN } };
    if (body !== undefined) {
        opts.headers['Content-Type'] = 'application/json';
        opts.body = typeof body === 'string' ? body : JSON.stringify(body);
    }
    let res;
    try {
        res = await fetch(path, opts);
    } catch {
        throw new Error('O FlashGerman parou de responder. Feche esta janela e abra o programa de novo.');
    }
    const text = await res.text();
    let data = null;
    try { data = text ? JSON.parse(text) : null; } catch { /* resposta não-JSON */ }
    if (!res.ok) throw new Error(data?.error || `Erro ${res.status}`);
    return data;
}

const storage = {
    get(key, fallback) {
        try { const v = localStorage.getItem('fg.' + key); return v === null ? fallback : JSON.parse(v); } catch { return fallback; }
    },
    set(key, value) {
        try { localStorage.setItem('fg.' + key, JSON.stringify(value)); } catch { /* armazenamento indisponível */ }
    },
};

let toastTimer = null;
function showToast(msg, isError = false) {
    const toast = $('toast');
    $('toastMsg').textContent = msg;
    $('toastIcon').className = isError
        ? 'fa-solid fa-triangle-exclamation text-red-400 text-base'
        : 'fa-solid fa-circle-check text-emerald-400 text-base';
    toast.classList.remove('translate-y-20', 'opacity-0');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => toast.classList.add('translate-y-20', 'opacity-0'), isError ? 6000 : 3200);
}

function openModal(id) { $(id).classList.remove('hidden'); }
function closeModal(id) { $(id).classList.add('hidden'); }

function formatBytes(n) {
    if (!n || n <= 0) return '';
    if (n >= 1024 ** 3) return (n / 1024 ** 3).toFixed(1).replace('.', ',') + ' GB';
    if (n >= 1024 ** 2) return Math.round(n / 1024 ** 2) + ' MB';
    return Math.round(n / 1024) + ' KB';
}

function formatDuration(sec) {
    sec = Math.max(0, Math.round(sec));
    if (sec < 60) return `${sec} s`;
    const m = Math.floor(sec / 60), s = sec % 60;
    if (m < 60) return s ? `${m} min ${s} s` : `${m} min`;
    return `${Math.floor(m / 60)} h ${m % 60} min`;
}

function openExternal(url) {
    api('/api/open-url', { method: 'POST', body: { url } }).catch((e) => showToast(e.message, true));
}

// ---------------------------------------------------------------------------
// Estado

const THEMES = [
    { key: 'supermarkt', label: '🛒 Einkauf & Supermarkt (Supermercado)', prompt: 'Einkauf & Supermarkt: compras no supermercado, padaria, caixa, devolução de garrafas (Pfand)', tag: 'Supermercado' },
    { key: 'restaurant', label: '☕ Café & Restaurant (Restaurante e Pedidos)', prompt: 'Café & Restaurant: pedir comida e bebida, reservar mesa, pedir a conta, gorjeta', tag: 'Restaurante_Café' },
    { key: 'arzt', label: '👨‍⚕️ Arzt & Gesundheit (Médico e Farmácia)', prompt: 'Arzt & Gesundheit: marcar consulta, descrever sintomas, farmácia (Apotheke), atestado (Krankschreibung)', tag: 'Médico_Saúde' },
    { key: 'verkehr', label: '🚆 Unterwegs & Transport (Ônibus, Trem e Rua)', prompt: 'Unterwegs & Transport: ônibus, trem (Deutsche Bahn), atrasos, passagens, pedir direções na rua', tag: 'Transporte_Rua' },
    { key: 'arbeit', label: '💼 Arbeit & Büro (Trabalho e Escritório)', prompt: 'Arbeit & Büro: reuniões, e-mails, colegas, pedir ajuda, prazos no escritório', tag: 'Trabalho_Büro' },
    { key: 'wohnung', label: '🏡 Wohnung & Nachbarn (Aluguel e Vizinhos)', prompt: 'Wohnung & Nachbarn: aluguel, proprietário (Vermieter), vizinhos, lixo e reciclagem, consertos', tag: 'Moradia_Wohnung' },
    { key: 'freizeit', label: '🍻 Freizeit & Freunde (Lazer e Conversa Leve)', prompt: 'Freizeit & Freunde: combinar programas, bares, esportes, conversa leve com amigos', tag: 'Lazer_Amigos' },
    { key: 'smalltalk', label: '💬 Smalltalk & Floskeln (Expressões do Dia a Dia)', prompt: 'Smalltalk & Floskeln: expressões prontas, reações, gírias leves e frases feitas do dia a dia', tag: 'Smalltalk_Gírias' },
    { key: 'behoerde', label: '🏛️ Bürgeramt & Behörden (Burocracia e Documentos)', prompt: 'Bürgeramt & Behörden: registro de endereço (Anmeldung), documentos, agendamentos, formulários', tag: 'Burocracia_Behörde' },
    { key: 'custom', label: '✨ Tema personalizado…', prompt: '', tag: 'Personalizado' },
];

const PHASES = {
    preparing: ['Preparando o pedido…', 'Montando as instruções para a IA.'],
    loading_model: ['Carregando a IA local…', 'Na primeira geração depois de abrir o app, o modelo é carregado na memória. Isso pode levar até 1 minuto.'],
    waiting: ['Enviando para a IA…', 'Aguardando o modelo começar a responder.'],
    thinking: ['A IA está pensando…', 'Alguns modelos raciocinam antes de escrever a resposta.'],
    generating: ['Criando frases hiper-naturais…', 'Escrevendo as frases, traduções e explicações.'],
    parsing: ['Organizando os cartões…', 'Quase pronto!'],
};

const state = {
    app: null,
    catalog: null,
    deck: [],
    view: storage.get('view', 'grid'),
    selectedProvider: null,
    models: [],
    suggested: '',
    jobId: null,
    jobPoll: null,
    lastParams: null,
    installPoll: null,
};

// ---------------------------------------------------------------------------
// Navegação entre telas

function showView(name) {
    for (const v of ['bootView', 'setupView', 'mainView']) $(v).classList.toggle('hidden', v !== name + 'View');
}

function setupStep(step) {
    showView('setup');
    $('stepChoose').classList.toggle('hidden', step !== 'choose');
    $('stepApi').classList.toggle('hidden', step !== 'api');
    $('stepLocal').classList.toggle('hidden', step !== 'local');
    if (step === 'choose') renderChoose();
    if (step === 'api') renderProviderList();
    if (step === 'local') renderLocal();
    window.scrollTo({ top: 0 });
}

async function refreshState() {
    state.app = await api('/api/state');
    renderHeader();
    return state.app;
}

function engineLabel() {
    const a = state.app?.active || {};
    if (!state.app?.configured) return null;
    if (a.kind === 'api') return { icon: 'fa-cloud', text: `${a.provider_name || a.provider_id} · ${a.model}` };
    if (a.kind === 'local') return { icon: 'fa-microchip', text: `Local · ${a.name}${a.gpu ? ' (GPU)' : ''}` };
    return null;
}

function renderHeader() {
    const label = engineLabel();
    $('enginePillText').textContent = label ? label.text : 'IA não configurada';
    $('enginePillIcon').className = `fa-solid ${label ? label.icon : 'fa-triangle-exclamation'} text-amber-400`;
    const upd = state.app?.update;
    if (upd?.available) {
        $('updateVersion').textContent = upd.latest;
        $('updateBanner').classList.remove('hidden');
    }
}

function renderChoose() {
    const label = engineLabel();
    $('currentConfigBox').classList.toggle('hidden', !label);
    if (label) $('currentConfigText').textContent = label.text;
}

// ---------------------------------------------------------------------------
// Configuração: APIs

const PRICING_GROUPS = [
    ['free', 'Gratuitos (com limites de uso)'],
    ['paid', 'Pagos (cobram por uso)'],
    ['local', 'Programas que você já tem instalados'],
    ['custom', 'Avançado'],
];

const BADGE_CLASS = {
    free: 'bg-emerald-500/10 text-emerald-400 border border-emerald-500/20',
    paid: 'bg-sky-500/10 text-sky-300 border border-sky-500/20',
    local: 'bg-violet-500/10 text-violet-300 border border-violet-500/20',
    custom: 'bg-slate-700/50 text-slate-300 border border-slate-600',
};

function renderProviderList() {
    const list = $('providerList');
    const saved = state.app?.providers || {};
    const active = state.app?.active || {};
    let html = '';
    for (const [pricing, title] of PRICING_GROUPS) {
        const items = state.catalog.providers.filter((p) => p.pricing === pricing);
        if (!items.length) continue;
        html += `<p class="text-[11px] font-semibold uppercase tracking-wider text-slate-500 pt-3 first:pt-0">${escapeHtml(title)}</p>`;
        for (const p of items) {
            const isSel = state.selectedProvider === p.id;
            const isActive = active.kind === 'api' && active.provider_id === p.id;
            const isSaved = saved[p.id]?.model;
            html += `
                <button type="button" data-provider="${escapeHtml(p.id)}" class="w-full text-left rounded-xl border px-4 py-3 transition flex items-center justify-between gap-3 ${isSel ? 'border-amber-400 bg-amber-500/5' : 'border-slate-800 bg-slate-900/80 hover:border-slate-600'}">
                    <span class="min-w-0">
                        <span class="block text-sm font-semibold text-slate-100 truncate">${escapeHtml(p.name)}</span>
                        <span class="block text-[11px] text-slate-500 truncate">${isActive ? '<i class="fa-solid fa-circle-check text-emerald-400 mr-1"></i>Em uso' : isSaved ? '<i class="fa-solid fa-check text-slate-400 mr-1"></i>Configurado' : escapeHtml(p.base_url ? new URL(p.base_url).host : 'endereço personalizado')}</span>
                    </span>
                    <span class="shrink-0 text-[10px] font-semibold uppercase px-2 py-1 rounded-md ${BADGE_CLASS[p.pricing] || BADGE_CLASS.custom}">${escapeHtml(p.badge)}</span>
                </button>`;
        }
    }
    list.innerHTML = html;
    if (!state.selectedProvider) {
        const act = active.kind === 'api' ? active.provider_id : null;
        if (act) selectProvider(act);
    }
}

function providerById(id) { return state.catalog.providers.find((p) => p.id === id); }

function setStatus(kind, html) {
    const el = $('pfStatus');
    if (!kind) { el.classList.add('hidden'); return; }
    const cls = {
        info: 'bg-slate-800/60 border-slate-700 text-slate-300',
        ok: 'bg-emerald-500/10 border-emerald-500/30 text-emerald-300',
        error: 'bg-red-500/10 border-red-500/30 text-red-300',
    }[kind];
    el.className = `text-xs rounded-xl p-3 border ${cls}`;
    el.innerHTML = html;
}

function selectProvider(id) {
    const p = providerById(id);
    if (!p) return;
    state.selectedProvider = id;
    state.models = [];
    state.suggested = '';
    renderProviderList();
    const saved = state.app?.providers?.[id];

    $('providerEmpty').classList.add('hidden');
    $('providerForm').classList.remove('hidden');
    $('pfName').textContent = p.name;
    $('pfDesc').textContent = p.description;
    $('pfBadge').textContent = p.badge;
    $('pfBadge').className = `shrink-0 text-[10px] font-semibold uppercase px-2 py-1 rounded-md ${BADGE_CLASS[p.pricing] || BADGE_CLASS.custom}`;

    $('pfKeyLinkBox').classList.toggle('hidden', !p.key_url);
    $('pfKeyLinkText').textContent = p.pricing === 'local'
        ? `Ainda não tem? Baixe o ${p.name.replace(/\s*\(.*\)/, '')} no site oficial.`
        : p.pricing === 'paid'
            ? 'Crie sua chave no site do provedor (é preciso adicionar créditos).'
            : 'Crie sua chave grátis no site do provedor. Leva 1 minuto.';
    $('pfKeyLink').textContent = p.pricing === 'local' ? 'Baixar' : 'Criar chave';

    const showUrl = p.editable_url || p.id === 'custom';
    $('pfUrlBox').classList.toggle('hidden', !showUrl);
    $('pfUrl').value = saved?.base_url || p.base_url || '';

    const showKey = p.needs_key || p.id === 'custom';
    $('pfKeyBox').classList.toggle('hidden', !showKey);
    $('pfKey').value = '';
    $('pfKey').type = 'password';
    $('pfKey').placeholder = saved?.has_key
        ? `Chave salva (${saved.key_hint}). Deixe em branco para manter.`
        : (p.key_hint || (p.needs_key ? 'Cole aqui a sua chave' : 'Opcional'));
    $('pfKeyHelp').textContent = p.needs_key ? 'Cole a chave inteira. Ao colar, a lista de modelos é carregada sozinha.' : 'Só preencha se o serviço exigir uma chave.';

    $('pfFreeOnlyBox').classList.toggle('hidden', !p.prefer_free);
    $('pfFreeOnlyBox').classList.toggle('flex', !!p.prefer_free);
    $('pfModelSearch').value = '';
    $('pfModelManual').value = '';
    $('pfModelTools').classList.add('hidden');
    const sel = $('pfModel');
    sel.innerHTML = saved?.model
        ? `<option value="${escapeHtml(saved.model)}">${escapeHtml(saved.model)} (salvo)</option>`
        : '<option value="">Clique em "Carregar lista de modelos"</option>';
    sel.disabled = !saved?.model;
    $('pfModelInfo').textContent = 'O melhor modelo para a tarefa é escolhido automaticamente, mas você pode trocar.';
    $('pfForget').classList.toggle('hidden', !saved);
    setStatus(null);

    if (saved?.has_key || (!p.needs_key && p.id !== 'custom')) loadModels(true);
}

async function loadModels(silent = false) {
    const p = providerById(state.selectedProvider);
    if (!p) return;
    const btn = $('pfLoadModels');
    btn.disabled = true;
    btn.innerHTML = '<i class="fa-solid fa-circle-notch animate-spin mr-1"></i>Carregando…';
    if (!silent) setStatus(null);
    try {
        const res = await api('/api/providers/models', {
            method: 'POST',
            body: { provider_id: p.id, base_url: $('pfUrl').value.trim(), api_key: $('pfKey').value.trim() },
        });
        state.models = res.models || [];
        state.suggested = res.suggested || '';
        $('pfModelTools').classList.remove('hidden');
        renderModelOptions();
        const hidden = (res.total || 0) - state.models.length;
        $('pfModelInfo').textContent = state.models.length
            ? `${state.models.length} modelos úteis encontrados${hidden > 0 ? ` (${hidden} de imagem, áudio etc. foram escondidos)` : ''}. ⭐ = recomendado.`
            : 'Nenhum modelo encontrado. Digite o nome manualmente abaixo.';
    } catch (e) {
        if (!silent || p.needs_key) setStatus('error', `<b>Não consegui listar os modelos.</b> ${escapeHtml(e.message)}`);
        else setStatus('info', `${escapeHtml(e.message)}`);
    } finally {
        btn.disabled = false;
        btn.innerHTML = '<i class="fa-solid fa-rotate mr-1"></i>Carregar lista de modelos';
    }
}

function renderModelOptions() {
    const p = providerById(state.selectedProvider);
    const sel = $('pfModel');
    const q = $('pfModelSearch').value.trim().toLowerCase();
    const freeOnly = p?.prefer_free && $('pfFreeOnly').checked;
    const saved = state.app?.providers?.[p.id]?.model;
    const current = sel.value || saved || state.suggested;
    let models = state.models.filter((m) => (!q || m.id.toLowerCase().includes(q) || (m.name || '').toLowerCase().includes(q)));
    if (freeOnly) models = models.filter((m) => m.free);
    if (!models.length) {
        sel.innerHTML = '<option value="">Nenhum modelo com esse filtro</option>';
        sel.disabled = true;
        return;
    }
    sel.disabled = false;
    sel.innerHTML = models.map((m) => {
        const tags = [m.id === state.suggested ? '⭐' : '', m.free ? '(grátis)' : ''].filter(Boolean).join(' ');
        const label = `${m.id}${m.name && m.name !== m.id ? ` · ${m.name}` : ''}${tags ? ' ' + tags : ''}`;
        return `<option value="${escapeHtml(m.id)}">${escapeHtml(label)}</option>`;
    }).join('');
    const pick = models.find((m) => m.id === current) || models.find((m) => m.id === state.suggested) || models[0];
    sel.value = pick.id;
}

async function saveProvider(ev) {
    ev.preventDefault();
    const p = providerById(state.selectedProvider);
    if (!p) return;
    const model = $('pfModelManual').value.trim() || $('pfModel').value;
    if (!model) {
        setStatus('error', 'Escolha um modelo (clique em <b>Carregar lista de modelos</b>) ou digite o nome manualmente.');
        return;
    }
    const btn = $('pfSave');
    btn.disabled = true;
    btn.querySelector('span').textContent = 'Testando a conexão…';
    setStatus('info', '<i class="fa-solid fa-circle-notch animate-spin mr-1"></i>Enviando uma mensagem de teste para o modelo…');
    try {
        await api('/api/providers/save', {
            method: 'POST',
            body: { provider_id: p.id, base_url: $('pfUrl').value.trim(), api_key: $('pfKey').value.trim(), model },
        });
        await refreshState();
        setStatus('ok', '<i class="fa-solid fa-circle-check mr-1"></i>Funcionou! Tudo pronto.');
        showToast(`Pronto! Usando ${p.name} · ${model}`);
        setTimeout(goMain, 600);
    } catch (e) {
        setStatus('error', escapeHtml(e.message));
    } finally {
        btn.disabled = false;
        btn.querySelector('span').textContent = 'Testar e começar a usar';
    }
}

async function forgetProvider() {
    const p = providerById(state.selectedProvider);
    if (!p || !confirm(`Apagar a chave e as configurações salvas de ${p.name}?`)) return;
    await api('/api/providers/forget', { method: 'POST', body: { provider_id: p.id } });
    await refreshState();
    selectProvider(p.id);
    showToast('Configuração apagada.');
}

// ---------------------------------------------------------------------------
// Configuração: IA local

function renderLocal() {
    const app = state.app;
    const local = app.local;
    const ramGB = app.ram_bytes ? app.ram_bytes / 1024 ** 3 : 0;
    const chips = [];
    if (ramGB) chips.push(`<i class="fa-solid fa-memory mr-1.5 text-emerald-400"></i>${Math.round(ramGB)} GB de RAM`);
    if (app.free_disk) chips.push(`<i class="fa-solid fa-hard-drive mr-1.5 text-emerald-400"></i>${formatBytes(app.free_disk)} livres`);
    chips.push(`<i class="fa-solid fa-computer mr-1.5 text-emerald-400"></i>${escapeHtml(local.platform)}`);
    if (local.engine.installed) chips.push(`<i class="fa-solid fa-gears mr-1.5 text-emerald-400"></i>Motor llama.cpp ${escapeHtml(local.engine.tag)} instalado`);
    $('sysInfo').innerHTML = chips.map((c) => `<span class="bg-slate-900 border border-slate-800 rounded-lg px-3 py-1.5 text-slate-300">${c}</span>`).join('');

    $('localUnsupported').classList.toggle('hidden', local.platform_ok);
    if (!local.platform_ok) {
        $('localUnsupported').textContent = `Ainda não há motor de IA local pronto para ${local.platform}. Use uma API ou o Ollama.`;
    }

    $('gpuBox').classList.toggle('hidden', !local.gpu_supported);
    $('gpuBox').classList.toggle('flex', local.gpu_supported);
    $('gpuToggle').checked = !!app.local_config?.gpu;

    const installed = local.installed || {};
    const activeId = app.mode === 'local' ? app.local_config?.model_id : null;
    const cards = state.catalog.local_models.map((m) => {
        const inst = installed[m.id];
        const lowRam = ramGB && ramGB + 0.6 < m.min_ram_gb; // 8 GB costumam aparecer como 7,8
        const inUse = activeId === m.id;
        return `
        <div class="bg-slate-900/90 border ${inUse ? 'border-emerald-400' : m.recommended ? 'border-amber-400/50' : 'border-slate-800'} rounded-2xl p-5 flex flex-col gap-3 shadow-xl">
            <div class="flex items-center justify-between gap-2">
                <span class="text-[10px] font-semibold uppercase px-2 py-1 rounded-md ${m.recommended ? 'bg-amber-500/10 text-amber-400 border border-amber-400/30' : 'bg-slate-800 text-slate-300 border border-slate-700'}">${escapeHtml(m.badge)}</span>
                ${inUse ? '<span class="text-[11px] text-emerald-400 font-semibold"><i class="fa-solid fa-circle-check mr-1"></i>Em uso</span>' : inst ? '<span class="text-[11px] text-slate-400"><i class="fa-solid fa-check mr-1"></i>Baixado</span>' : ''}
            </div>
            <div>
                <h3 class="font-display font-bold text-lg text-white">${escapeHtml(m.name)}</h3>
                <p class="text-xs text-slate-400 mt-1 leading-relaxed">${escapeHtml(m.description)}</p>
            </div>
            <div class="text-[11px] text-slate-400 space-y-1">
                <p><i class="fa-solid fa-download w-4 text-slate-500"></i> Download de ~${String(m.approx_size_gb).replace('.', ',')} GB</p>
                <p><i class="fa-solid fa-memory w-4 text-slate-500"></i> Recomendado: ${m.min_ram_gb} GB de RAM ou mais</p>
                <p><i class="fa-solid fa-scale-balanced w-4 text-slate-500"></i> Licença ${escapeHtml(m.license)}</p>
            </div>
            ${lowRam ? `<p class="text-[11px] text-amber-300 bg-amber-500/10 border border-amber-500/20 rounded-lg p-2"><i class="fa-solid fa-triangle-exclamation mr-1"></i>Seu computador tem ${Math.round(ramGB)} GB: pode ficar muito lento ou não abrir.</p>` : ''}
            <div class="mt-auto flex items-center gap-2 pt-1">
                ${inst
                    ? `<button type="button" data-local-use="${escapeHtml(m.id)}" class="flex-1 bg-emerald-500 hover:bg-emerald-400 text-slate-950 font-bold text-sm py-2.5 rounded-xl transition">${inUse ? 'Continuar usando' : 'Usar este'}</button>
                       <button type="button" data-local-delete="${escapeHtml(m.id)}" title="Apagar do computador" class="h-10 w-10 rounded-xl border border-slate-700 text-slate-400 hover:text-red-300 hover:border-red-500/40"><i class="fa-solid fa-trash-can"></i></button>`
                    : `<button type="button" data-local-install="${escapeHtml(m.id)}" class="flex-1 ${m.recommended ? 'bg-gradient-to-r from-emerald-500 to-emerald-600 hover:from-emerald-400 hover:to-emerald-500 text-slate-950' : 'bg-slate-800 hover:bg-slate-700 text-slate-100 border border-slate-700'} font-bold text-sm py-2.5 rounded-xl transition"><i class="fa-solid fa-download mr-1.5"></i>Baixar e usar</button>`}
            </div>
        </div>`;
    });
    for (const [id, m] of Object.entries(installed)) {
        if (!id.startsWith('custom:')) continue;
        const inUse = activeId === id;
        cards.push(`
        <div class="bg-slate-900/90 border ${inUse ? 'border-emerald-400' : 'border-slate-800'} rounded-2xl p-5 flex flex-col gap-3">
            <span class="text-[10px] font-semibold uppercase px-2 py-1 rounded-md bg-slate-800 text-slate-300 border border-slate-700 w-max">Personalizado</span>
            <h3 class="font-display font-bold text-sm text-white break-all">${escapeHtml(m.name)}</h3>
            <p class="text-[11px] text-slate-400 break-all">${escapeHtml(m.file)} · ${formatBytes(m.size)}</p>
            <div class="mt-auto flex items-center gap-2">
                <button type="button" data-local-use="${escapeHtml(id)}" class="flex-1 bg-emerald-500 hover:bg-emerald-400 text-slate-950 font-bold text-sm py-2.5 rounded-xl">${inUse ? 'Continuar usando' : 'Usar este'}</button>
                <button type="button" data-local-delete="${escapeHtml(id)}" class="h-10 w-10 rounded-xl border border-slate-700 text-slate-400 hover:text-red-300"><i class="fa-solid fa-trash-can"></i></button>
            </div>
        </div>`);
    }
    $('localModels').innerHTML = cards.join('');
    updateInstallPanel(local.install);
    if (local.install?.active) pollInstall();
}

async function startInstall(modelId, repo = '') {
    try {
        await api('/api/local/install', { method: 'POST', body: { model_id: modelId, repo, gpu: $('gpuToggle').checked } });
        updateInstallPanel({ active: true, stage: 'engine', label: 'Preparando…' });
        pollInstall();
    } catch (e) {
        showToast(e.message, true);
    }
}

async function useLocal(modelId) {
    try {
        await api('/api/local/use', { method: 'POST', body: { model_id: modelId, gpu: $('gpuToggle').checked } });
        await refreshState();
        showToast('IA local ativada!');
        goMain();
    } catch {
        // Ex.: trocou para GPU e o motor dessa versão ainda não foi baixado.
        startInstall(modelId);
    }
}

async function deleteLocal(modelId) {
    if (!confirm('Apagar este modelo do computador? Você pode baixá-lo de novo depois.')) return;
    try {
        await api('/api/local/delete', { method: 'POST', body: { model_id: modelId } });
        await refreshState();
        renderLocal();
        showToast('Modelo apagado.');
    } catch (e) {
        showToast(e.message, true);
    }
}

function updateInstallPanel(inst) {
    const panel = $('installPanel');
    if (!inst || inst.stage === 'idle') { panel.classList.add('hidden'); return; }
    panel.classList.remove('hidden');
    $('installLabel').textContent = inst.label || '';
    const pct = inst.total > 0 ? Math.min(100, (inst.done / inst.total) * 100) : (inst.stage === 'done' ? 100 : 0);
    $('installBar').style.width = pct.toFixed(1) + '%';
    $('installBytes').textContent = inst.total > 0 ? `${formatBytes(inst.done)} de ${formatBytes(inst.total)} (${pct.toFixed(0)}%)` : (inst.done > 0 ? formatBytes(inst.done) : '');
    $('installEta').textContent = inst.active && inst.speed > 0
        ? `${formatBytes(inst.speed)}/s${inst.total > 0 ? ` · faltam ~${formatDuration((inst.total - inst.done) / inst.speed)}` : ''}`
        : '';
    $('installCancel').classList.toggle('hidden', !inst.active);
    $('installError').classList.toggle('hidden', inst.stage !== 'error');
    $('installError').textContent = inst.error || '';
    const icon = $('installIcon');
    icon.className = inst.active
        ? 'fa-solid fa-circle-notch animate-spin text-emerald-400'
        : inst.stage === 'done' ? 'fa-solid fa-circle-check text-emerald-400'
            : inst.stage === 'error' ? 'fa-solid fa-triangle-exclamation text-red-400'
                : 'fa-solid fa-pause text-amber-400';
    document.querySelectorAll('[data-local-install],[data-local-use],#customRepoBtn').forEach((b) => { b.disabled = !!inst.active; b.classList.toggle('opacity-50', !!inst.active); });
}

function pollInstall() {
    clearTimeout(state.installPoll);
    const tick = async () => {
        let st;
        try { st = await api('/api/local/status'); } catch { state.installPoll = setTimeout(tick, 2000); return; }
        updateInstallPanel(st.install);
        if (st.install.active) {
            state.installPoll = setTimeout(tick, 700);
            return;
        }
        await refreshState();
        if (st.install.stage === 'done') {
            showToast('IA local instalada! Pode gerar suas frases.');
            setTimeout(goMain, 900);
        } else if (!$('stepLocal').classList.contains('hidden')) {
            renderLocal();
        }
    };
    state.installPoll = setTimeout(tick, 500);
}

// ---------------------------------------------------------------------------
// Gerador

function goMain() {
    showView('main');
    renderDeck();
}

function currentParams() {
    const themeKey = $('themeSelect').value;
    const theme = THEMES.find((t) => t.key === themeKey) || THEMES[0];
    let prompt = theme.prompt;
    let themeLabel = theme.tag.replace(/_/g, ' ');
    let themeTag = theme.tag;
    if (themeKey === 'custom') {
        prompt = $('customThemeInput').value.trim();
        themeLabel = prompt;
        themeTag = prompt.slice(0, 40);
    }
    return {
        request: {
            theme: prompt,
            tense: $('tenseSelect').value,
            level: document.querySelector('input[name="level"]:checked').value,
            count: parseInt($('countRange').value, 10),
            colloquial: $('includeColloquial').checked,
            pronunciation: $('includePronunciation').checked,
        },
        themeLabel,
        themeTag,
    };
}

function saveOptions() {
    storage.set('options', {
        theme: $('themeSelect').value,
        custom: $('customThemeInput').value,
        tense: $('tenseSelect').value,
        level: document.querySelector('input[name="level"]:checked').value,
        count: $('countRange').value,
        colloquial: $('includeColloquial').checked,
        pronunciation: $('includePronunciation').checked,
    });
}

function restoreOptions() {
    $('themeSelect').innerHTML = THEMES.map((t) => `<option value="${t.key}">${escapeHtml(t.label)}</option>`).join('');
    const o = storage.get('options', null);
    if (o) {
        if (THEMES.some((t) => t.key === o.theme)) $('themeSelect').value = o.theme;
        $('customThemeInput').value = o.custom || '';
        if (o.tense) $('tenseSelect').value = o.tense;
        const lvl = document.querySelector(`input[name="level"][value="${CSS.escape(o.level || 'A1-A2')}"]`);
        if (lvl) lvl.checked = true;
        if (o.count) $('countRange').value = o.count;
        $('includeColloquial').checked = o.colloquial !== false;
        $('includePronunciation').checked = o.pronunciation !== false;
    }
    $('countDisplay').textContent = `${$('countRange').value} frases`;
    $('customThemeContainer').classList.toggle('hidden', $('themeSelect').value !== 'custom');
}

async function generatePhrases(params = null) {
    if (state.jobId) return;
    if (!state.app?.configured) {
        showToast('Configure a IA primeiro.', true);
        setupStep('choose');
        return;
    }
    const p = params || currentParams();
    if (!p.request.theme) {
        showToast('Por favor, digite seu tema personalizado.', true);
        $('customThemeInput').focus();
        return;
    }
    state.lastParams = p;
    saveOptions();
    hideError();

    let job;
    try {
        job = await api('/api/generate', { method: 'POST', body: p.request });
    } catch (e) {
        showError(e.message);
        return;
    }
    state.jobId = job.id;
    $('generateBtn').disabled = true;
    $('emptyState').classList.add('hidden');
    $('loadingState').classList.remove('hidden');
    $('loadingState').classList.add('flex');
    updateLoading(job);

    const tick = async () => {
        let j;
        try {
            j = await api(`/api/jobs/${encodeURIComponent(state.jobId)}`);
        } catch (e) {
            finishJob();
            showError(e.message);
            return;
        }
        updateLoading(j);
        if (j.status === 'running') {
            state.jobPoll = setTimeout(tick, 700);
            return;
        }
        finishJob();
        if (j.status === 'done') {
            const now = Date.now();
            const newCards = (j.cards || []).map((c, i) => ({
                id: (crypto.randomUUID ? crypto.randomUUID() : `${now}_${i}`),
                german: c.german,
                pt: c.pt,
                literal: c.literal || '',
                tense: c.tense || 'Geral',
                grammar: c.grammar || '',
                pronunciation: p.request.pronunciation ? (c.pronunciation || '') : '',
                context: c.context || '',
                theme: p.themeLabel,
                themeTag: p.themeTag,
                level: p.request.level,
                createdAt: now,
            }));
            state.deck = [...newCards, ...state.deck];
            saveDeck();
            renderDeck();
            showToast(j.notice ? `${newCards.length} frases adicionadas. ${j.notice}` : `${newCards.length} novas frases adicionadas!`);
        } else if (j.status === 'cancelled') {
            showToast('Geração cancelada.');
            renderDeck();
        } else {
            showError(j.error || 'Erro ao gerar frases.');
            renderDeck();
        }
    };
    state.jobPoll = setTimeout(tick, 600);
}

function updateLoading(j) {
    const [title, text] = PHASES[j.phase] || PHASES.preparing;
    $('loadingTitle').textContent = title;
    $('loadingText').textContent = text;
    const expected = j.expected || 1;
    const found = Math.min(j.found || 0, expected);
    let pct = (found / expected) * 100;
    if (j.phase === 'parsing') pct = 100;
    $('loadingBar').style.width = Math.max(j.phase === 'generating' ? 4 : 0, pct).toFixed(0) + '%';
    $('loadingCount').textContent = j.phase === 'generating' || j.phase === 'parsing' ? `${found} de ${expected} frases` : `${expected} frases pedidas`;
    $('loadingElapsed').textContent = formatDuration((j.elapsed_ms || 0) / 1000);
}

function finishJob() {
    clearTimeout(state.jobPoll);
    state.jobId = null;
    $('generateBtn').disabled = false;
    $('loadingState').classList.add('hidden');
    $('loadingState').classList.remove('flex');
}

async function cancelGeneration() {
    if (!state.jobId) return;
    try { await api(`/api/jobs/${encodeURIComponent(state.jobId)}/cancel`, { method: 'POST' }); } catch { /* já terminou */ }
}

function showError(msg) {
    $('errorText').textContent = msg;
    $('errorBox').classList.remove('hidden');
    $('errorBox').classList.add('flex');
}
function hideError() {
    $('errorBox').classList.add('hidden');
    $('errorBox').classList.remove('flex');
}

// ---------------------------------------------------------------------------
// Baralho

let saveTimer = null;
function saveDeck() {
    clearTimeout(saveTimer);
    saveTimer = setTimeout(async () => {
        try {
            await api('/api/deck', { method: 'PUT', body: JSON.stringify(state.deck) });
        } catch (e) {
            showToast('Não foi possível salvar o baralho: ' + e.message, true);
        }
    }, 300);
}

async function loadDeck() {
    try {
        const deck = await api('/api/deck');
        state.deck = Array.isArray(deck) ? deck.filter((c) => c && c.german && c.id) : [];
    } catch {
        state.deck = [];
    }
}

function filteredDeck() {
    const q = $('searchInput').value.toLowerCase().trim();
    if (!q) return state.deck;
    return state.deck.filter((c) => [c.german, c.pt, c.grammar, c.tense, c.theme, c.context].some((v) => String(v || '').toLowerCase().includes(q)));
}

function renderDeck() {
    const total = state.deck.length;
    $('badgeCount').textContent = total;
    $('deckStatsText').textContent = `${total} ${total === 1 ? 'frase pronta' : 'frases prontas'} para exportar`;
    for (const id of ['exportCsvBtn', 'copyAnkiBtn', 'clearBtn']) $(id).disabled = total === 0;

    const generating = !!state.jobId;
    const grid = $('gridContainer');
    const table = $('tableContainer');
    $('emptyState').classList.toggle('hidden', total > 0 || generating);
    if (total === 0) {
        grid.classList.add('hidden');
        table.classList.add('hidden');
        return;
    }

    const cards = filteredDeck();
    const isGrid = state.view === 'grid';
    $('viewGridBtn').className = `px-3 py-1 text-xs font-medium rounded-md transition ${isGrid ? 'bg-amber-500 text-slate-950' : 'text-slate-400 hover:text-white'}`;
    $('viewTableBtn').className = `px-3 py-1 text-xs font-medium rounded-md transition ${!isGrid ? 'bg-amber-500 text-slate-950' : 'text-slate-400 hover:text-white'}`;
    grid.classList.toggle('hidden', !isGrid);
    table.classList.toggle('hidden', isGrid);

    if (!cards.length) {
        const msg = '<p class="text-xs text-slate-500 p-6 text-center col-span-full">Nenhuma frase encontrada com esse filtro.</p>';
        if (isGrid) grid.innerHTML = msg; else $('tableBody').innerHTML = `<tr><td colspan="5">${msg}</td></tr>`;
        return;
    }

    if (isGrid) {
        grid.innerHTML = cards.map((c) => {
            const id = escapeHtml(c.id);
            return `
            <div class="card-flip-container h-[310px] w-full cursor-pointer group" data-card="${id}" tabindex="0" role="button" aria-label="Virar cartão">
                <div class="card-flip-inner relative w-full h-full rounded-2xl shadow-lg">
                    <div class="card-front absolute inset-0 bg-slate-900 border border-slate-800 group-hover:border-amber-500/40 rounded-2xl p-5 flex flex-col justify-between">
                        <div class="min-h-0">
                            <div class="flex items-center justify-between mb-3 gap-2">
                                <div class="flex items-center gap-1.5 min-w-0">
                                    <span class="bg-amber-500/10 text-amber-400 border border-amber-400/20 px-2 py-0.5 rounded-md font-semibold text-[10px] uppercase shrink-0">${escapeHtml(c.tense)}</span>
                                    <span class="bg-slate-800 text-slate-300 px-2 py-0.5 rounded-md font-medium text-[10px] truncate">${escapeHtml(c.theme)}</span>
                                </div>
                                <button type="button" data-action="speak" data-id="${id}" title="Ouvir pronúncia" class="h-8 w-8 shrink-0 rounded-lg bg-slate-800 hover:bg-slate-700 flex items-center justify-center transition border border-slate-700">
                                    <i class="fa-solid fa-volume-high text-amber-400"></i>
                                </button>
                            </div>
                            <h4 class="font-display font-bold text-lg sm:text-xl text-slate-100 leading-snug mt-2 line-clamp-5">${escapeHtml(c.german)}</h4>
                            ${c.pronunciation ? `<p class="text-xs text-slate-400 italic mt-2 flex items-start gap-1.5 line-clamp-3"><i class="fa-solid fa-comment-dots text-slate-500 text-[10px] mt-1"></i><span>${escapeHtml(c.pronunciation)}</span></p>` : ''}
                        </div>
                        <div class="border-t border-slate-800/80 pt-3 flex items-center justify-between text-xs text-slate-500">
                            <span class="text-[11px]"><i class="fa-solid fa-rotate text-amber-400/80 mr-1"></i>Clique para ver a tradução</span>
                            <div class="flex gap-3">
                                <button type="button" data-action="edit" data-id="${id}" class="hover:text-amber-400 transition" title="Editar"><i class="fa-solid fa-pen"></i></button>
                                <button type="button" data-action="delete" data-id="${id}" class="hover:text-red-400 transition" title="Excluir"><i class="fa-solid fa-trash-can"></i></button>
                            </div>
                        </div>
                    </div>
                    <div class="card-back absolute inset-0 bg-slate-900 border border-amber-500/30 rounded-2xl p-5 flex flex-col justify-between">
                        <div class="space-y-3 overflow-y-auto pr-1 min-h-0">
                            <div class="flex items-center justify-between">
                                <span class="bg-emerald-500/10 text-emerald-400 border border-emerald-500/20 px-2 py-0.5 rounded-md font-semibold text-[10px] uppercase">Português (PT-BR)</span>
                                <span class="text-[10px] text-slate-500">${escapeHtml(c.level)}</span>
                            </div>
                            <p class="font-display font-bold text-base text-emerald-400 leading-snug">${escapeHtml(c.pt)}</p>
                            ${c.literal ? `<p class="text-[11px] text-slate-500">Literal: <i>${escapeHtml(c.literal)}</i></p>` : ''}
                            ${c.grammar ? `<div class="bg-slate-950 p-2.5 rounded-xl border border-slate-800 text-xs text-slate-300 space-y-0.5"><span class="text-amber-400 font-semibold text-[10px] uppercase block"><i class="fa-solid fa-lightbulb mr-1"></i>Dica gramatical</span><p class="text-[11px] leading-relaxed text-slate-300">${escapeHtml(c.grammar)}</p></div>` : ''}
                            ${c.context ? `<div class="text-[11px] text-slate-400 bg-slate-950/60 p-2 rounded-lg border border-slate-800/60 italic"><span class="text-slate-500 font-normal not-italic block text-[10px]">Contexto:</span>${escapeHtml(c.context)}</div>` : ''}
                        </div>
                        <div class="border-t border-slate-800/80 pt-2 flex items-center justify-between text-xs text-slate-500">
                            <span class="text-[11px]"><i class="fa-solid fa-rotate text-amber-400/80 mr-1"></i>Clique para voltar</span>
                            <button type="button" data-action="delete" data-id="${id}" class="hover:text-red-400 transition" title="Excluir"><i class="fa-solid fa-trash-can"></i></button>
                        </div>
                    </div>
                </div>
            </div>`;
        }).join('');
    } else {
        $('tableBody').innerHTML = cards.map((c, idx) => {
            const id = escapeHtml(c.id);
            return `
            <tr class="hover:bg-slate-800/40 transition align-top">
                <td class="p-3 text-center text-slate-500">${idx + 1}</td>
                <td class="p-3 font-semibold text-slate-100">
                    <div class="flex items-start gap-2">
                        <span>${escapeHtml(c.german)}</span>
                        <button type="button" data-action="speak" data-id="${id}" class="text-slate-500 hover:text-amber-400" title="Ouvir"><i class="fa-solid fa-volume-high"></i></button>
                    </div>
                    ${c.pronunciation ? `<span class="block text-[10px] text-slate-400 italic font-normal mt-1">${escapeHtml(c.pronunciation)}</span>` : ''}
                </td>
                <td class="p-3 text-emerald-400 font-medium">${escapeHtml(c.pt)}</td>
                <td class="p-3">
                    <span class="bg-amber-500/10 text-amber-400 px-2 py-0.5 rounded text-[10px] font-semibold uppercase block w-max mb-1">${escapeHtml(c.tense)}</span>
                    <span class="text-[10px] text-slate-400 block">${escapeHtml(c.grammar || '-')}</span>
                </td>
                <td class="p-3 text-center">
                    <div class="flex items-center justify-center gap-3 text-slate-400">
                        <button type="button" data-action="edit" data-id="${id}" class="hover:text-amber-400" title="Editar"><i class="fa-solid fa-pen"></i></button>
                        <button type="button" data-action="delete" data-id="${id}" class="hover:text-red-400" title="Excluir"><i class="fa-solid fa-trash-can"></i></button>
                    </div>
                </td>
            </tr>`;
        }).join('');
    }
}

function handleDeckClick(ev) {
    const btn = ev.target.closest('button[data-action]');
    if (btn) {
        ev.stopPropagation();
        const card = state.deck.find((c) => c.id === btn.dataset.id);
        if (!card) return;
        if (btn.dataset.action === 'speak') speakGerman(card.german, btn);
        if (btn.dataset.action === 'edit') openEditCard(card);
        if (btn.dataset.action === 'delete') deleteCard(card.id);
        return;
    }
    const flip = ev.target.closest('[data-card]');
    if (flip) flip.classList.toggle('flipped');
}

function deleteCard(id) {
    state.deck = state.deck.filter((c) => c.id !== id);
    saveDeck();
    renderDeck();
    showToast('Frase removida do baralho.');
}

function clearDeck() {
    if (!confirm('Tem certeza de que deseja apagar todas as frases do baralho? Exporte antes se quiser guardar.')) return;
    state.deck = [];
    saveDeck();
    renderDeck();
    showToast('Baralho limpo.');
}

function openEditCard(card) {
    $('editCardId').value = card.id;
    $('editGerman').value = card.german;
    $('editPt').value = card.pt;
    $('editPronunciation').value = card.pronunciation || '';
    $('editGrammar').value = card.grammar || '';
    $('editContext').value = card.context || '';
    openModal('editModal');
    $('editGerman').focus();
}

function saveCardEdit(ev) {
    ev.preventDefault();
    const card = state.deck.find((c) => c.id === $('editCardId').value);
    if (!card) return;
    const german = $('editGerman').value.trim();
    const pt = $('editPt').value.trim();
    if (!german || !pt) {
        showToast('A frase em alemão e a tradução não podem ficar vazias.', true);
        return;
    }
    Object.assign(card, {
        german, pt,
        pronunciation: $('editPronunciation').value.trim(),
        grammar: $('editGrammar').value.trim(),
        context: $('editContext').value.trim(),
    });
    saveDeck();
    renderDeck();
    closeModal('editModal');
    showToast('Cartão atualizado!');
}

function exportCSV() {
    if (!state.deck.length) return;
    const blob = new Blob([buildAnkiCsv(state.deck)], { type: 'text/csv;charset=utf-8' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `FlashGerman_${new Date().toISOString().slice(0, 10)}.csv`;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 5000);
    showToast('Arquivo CSV salvo na pasta de Downloads!');
}

async function copyTsv() {
    if (!state.deck.length) return;
    const text = buildTsv(state.deck);
    try {
        await navigator.clipboard.writeText(text);
    } catch {
        const ta = document.createElement('textarea');
        ta.value = text;
        document.body.appendChild(ta);
        ta.select();
        document.execCommand('copy');
        ta.remove();
    }
    showToast('Copiado! Cole numa planilha ou em outro app de flashcards.');
}

// ---------------------------------------------------------------------------
// Pronúncia (voz do próprio sistema)

let germanVoice = null;
let warnedNoVoice = false;
function pickVoice() {
    const voices = window.speechSynthesis?.getVoices?.() || [];
    germanVoice = voices.find((v) => v.lang === 'de-DE' && /natural|online|google/i.test(v.name))
        || voices.find((v) => v.lang === 'de-DE')
        || voices.find((v) => v.lang?.toLowerCase().startsWith('de'))
        || null;
}

function speakGerman(text, btn) {
    if (!('speechSynthesis' in window)) {
        showToast('Este navegador não tem síntese de voz.', true);
        return;
    }
    if (!germanVoice) pickVoice();
    if (!germanVoice && !warnedNoVoice) {
        warnedNoVoice = true;
        showToast('Nenhuma voz em alemão instalada no sistema; a pronúncia pode sair errada. Instale o idioma alemão nas configurações de fala do sistema.', true);
    }
    window.speechSynthesis.cancel();
    const u = new SpeechSynthesisUtterance(text);
    u.lang = 'de-DE';
    if (germanVoice) u.voice = germanVoice;
    u.rate = 0.9;
    const icon = btn?.querySelector('i');
    if (icon) {
        icon.className = 'fa-solid fa-spinner animate-spin text-amber-400';
        const reset = () => { icon.className = 'fa-solid fa-volume-high text-amber-400'; };
        u.onend = reset;
        u.onerror = reset;
    }
    window.speechSynthesis.speak(u);
}

// ---------------------------------------------------------------------------
// Ciclo de vida: avisa o app que a janela está aberta/fechou

function heartbeat() {
    api('/api/heartbeat', { method: 'POST', body: { client_id: CLIENT_ID } }).catch(() => {});
}

function busy() {
    return !!state.jobId || !$('installPanel').classList.contains('hidden') && !$('installCancel').classList.contains('hidden');
}

async function quitApp() {
    const msg = busy()
        ? 'Há um download ou geração em andamento. Fechar mesmo assim? (Downloads continuam de onde pararam na próxima vez.)'
        : 'Fechar o FlashGerman?';
    if (!confirm(msg)) return;
    try { await api('/api/shutdown', { method: 'POST' }); } catch { /* já fechou */ }
    $('byeOverlay').classList.remove('hidden');
    setTimeout(() => window.close(), 400);
}

// ---------------------------------------------------------------------------
// Ligações de eventos

function bindEvents() {
    document.addEventListener('click', (ev) => {
        const open = ev.target.closest('[data-open-modal]');
        if (open) openModal(open.dataset.openModal);
        const close = ev.target.closest('[data-close-modal]');
        if (close) close.closest('[data-modal]').classList.add('hidden');
        const modalBg = ev.target.matches('[data-modal]') ? ev.target : null;
        if (modalBg) modalBg.classList.add('hidden');
        const go = ev.target.closest('[data-goto]');
        if (go) setupStep(go.dataset.goto);
    });
    document.addEventListener('keydown', (ev) => {
        if (ev.key === 'Escape') document.querySelectorAll('[data-modal]').forEach((m) => m.classList.add('hidden'));
        if (ev.key === 'Enter' && ev.target.matches('[data-card]')) ev.target.classList.toggle('flipped');
    });

    $('settingsBtn').addEventListener('click', () => setupStep('choose'));
    $('enginePill').addEventListener('click', () => setupStep('choose'));
    $('backToMainBtn').addEventListener('click', goMain);
    $('quitBtn').addEventListener('click', quitApp);
    $('updateBtn').addEventListener('click', () => openExternal(state.app?.update?.url || `https://github.com/${state.app?.repo}/releases`));
    $('repoLink').addEventListener('click', () => openExternal(`https://github.com/${state.app?.repo || 'MaloneFreak/FlashGerman'}`));
    $('openDataDir').addEventListener('click', () => api('/api/open-data-dir', { method: 'POST' }).catch((e) => showToast(e.message, true)));

    // Provedores
    $('providerList').addEventListener('click', (ev) => {
        const b = ev.target.closest('[data-provider]');
        if (b) selectProvider(b.dataset.provider);
    });
    $('pfKeyLink').addEventListener('click', () => {
        const p = providerById(state.selectedProvider);
        if (p?.key_url) openExternal(p.key_url);
    });
    $('pfKeyToggle').addEventListener('click', () => {
        const k = $('pfKey');
        k.type = k.type === 'password' ? 'text' : 'password';
    });
    $('pfLoadModels').addEventListener('click', () => loadModels(false));
    $('pfKey').addEventListener('paste', () => setTimeout(() => { if ($('pfKey').value.trim().length > 10) loadModels(false); }, 50));
    $('pfModelSearch').addEventListener('input', renderModelOptions);
    $('pfFreeOnly').addEventListener('change', renderModelOptions);
    $('providerForm').addEventListener('submit', saveProvider);
    $('pfForget').addEventListener('click', forgetProvider);

    // IA local
    $('localModels').addEventListener('click', (ev) => {
        const inst = ev.target.closest('[data-local-install]');
        if (inst) startInstall(inst.dataset.localInstall);
        const use = ev.target.closest('[data-local-use]');
        if (use) useLocal(use.dataset.localUse);
        const del = ev.target.closest('[data-local-delete]');
        if (del) deleteLocal(del.dataset.localDelete);
    });
    $('installCancel').addEventListener('click', () => api('/api/local/cancel', { method: 'POST' }).catch(() => {}));
    $('customRepoBtn').addEventListener('click', () => {
        const repo = $('customRepo').value.trim();
        if (!repo) { showToast('Digite o repositório (usuario/modelo-GGUF).', true); return; }
        startInstall('custom', repo);
    });

    // Gerador
    $('themeSelect').addEventListener('change', () => {
        const custom = $('themeSelect').value === 'custom';
        $('customThemeContainer').classList.toggle('hidden', !custom);
        if (custom) $('customThemeInput').focus();
    });
    $('countRange').addEventListener('input', () => { $('countDisplay').textContent = `${$('countRange').value} frases`; });
    $('generateBtn').addEventListener('click', () => generatePhrases());
    $('customThemeInput').addEventListener('keydown', (ev) => { if (ev.key === 'Enter') generatePhrases(); });
    $('cancelGenBtn').addEventListener('click', cancelGeneration);
    $('errorRetry').addEventListener('click', () => generatePhrases(state.lastParams));
    $('errorSettings').addEventListener('click', () => { hideError(); setupStep('choose'); });
    $('errorClose').addEventListener('click', hideError);
    $('searchInput').addEventListener('input', renderDeck);
    $('viewGridBtn').addEventListener('click', () => { state.view = 'grid'; storage.set('view', 'grid'); renderDeck(); });
    $('viewTableBtn').addEventListener('click', () => { state.view = 'table'; storage.set('view', 'table'); renderDeck(); });
    $('gridContainer').addEventListener('click', handleDeckClick);
    $('tableBody').addEventListener('click', handleDeckClick);
    $('exportCsvBtn').addEventListener('click', exportCSV);
    $('copyAnkiBtn').addEventListener('click', copyTsv);
    $('clearBtn').addEventListener('click', clearDeck);
    $('editForm').addEventListener('submit', saveCardEdit);

    // Janela
    window.addEventListener('beforeunload', (ev) => {
        if (busy()) { ev.preventDefault(); ev.returnValue = ''; }
    });
    window.addEventListener('pagehide', () => {
        navigator.sendBeacon?.(`/api/bye?token=${encodeURIComponent(TOKEN)}&client=${encodeURIComponent(CLIENT_ID)}`);
    });
    document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'visible') heartbeat(); });
    if (window.speechSynthesis) {
        pickVoice();
        window.speechSynthesis.addEventListener?.('voiceschanged', pickVoice);
    }
}

// ---------------------------------------------------------------------------
// Início

async function boot() {
    $('appVersion').textContent = VERSION && VERSION !== 'dev' ? VERSION : '';
    bindEvents();
    restoreOptions();
    heartbeat();
    setInterval(heartbeat, 20000);
    try {
        const [, catalog] = await Promise.all([refreshState(), api('/api/catalog'), loadDeck()]);
        state.catalog = catalog;
    } catch (e) {
        showView('main');
        showError(e.message);
        return;
    }
    if (state.app.local?.install?.active) {
        setupStep('local');
    } else if (!state.app.configured) {
        setupStep('choose');
    } else {
        goMain();
    }
}

boot();
