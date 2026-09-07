// Top-level app — tabs + tweaks panel

const SURFACES = [
  {
    id: 'editor',
    n: 1,
    title: 'Editor',
    intent: 'how a dev writes a .fct file in their IDE',
    variants: [
      { n: 1, title: 'Classic IDE', sub: 'tree · code · contract inspector', tag: 'IDE plugin', comp: 'EditorV1' },
      { n: 2, title: 'Source ↔ Output', sub: 'see compiled Go template change as you type', tag: 'split view', comp: 'EditorV2' },
      { n: 3, title: 'Contract Notebook', sub: 'contract as a table; child facets as pills', tag: 'novel', comp: 'EditorV3' },
    ],
  },
  {
    id: 'playground',
    n: 2,
    title: 'Playground',
    intent: 'fct.dev/play — paste .fct, share, see all generated artifacts',
    variants: [
      { n: 1, title: '3-pane (canonical)', sub: 'source · AST · output (tabbed)', tag: 'familiar', comp: 'PlaygroundV1' },
      { n: 2, title: 'Pipeline waterfall', sub: 'parse → resolve → check → codegen → preview', tag: 'pedagogical', comp: 'PlaygroundV2' },
      { n: 3, title: 'Preview-first', sub: 'big rendered facet; source + Go below', tag: 'designerly', comp: 'PlaygroundV3' },
    ],
  },
  {
    id: 'devtools',
    n: 3,
    title: 'Live DevTools',
    intent: 'the FA Live moment — server-authoritative stream + facet tree',
    variants: [
      { n: 1, title: 'Tree + Inspector', sub: 'react-devtools shape, stream pinned to bottom', tag: 'most familiar', comp: 'DevToolsV1' },
      { n: 2, title: 'Stream-first', sub: 'Sitra Achra timeline IS the page; tree = filter', tag: 'philosophy-forward', comp: 'DevToolsV2' },
      { n: 3, title: 'Spatial map', sub: 'facets on a canvas; signals as arrows', tag: 'wild card', comp: 'DevToolsV3' },
    ],
  },
  {
    id: 'registry',
    n: 4,
    title: 'Registry — fct.dev',
    intent: 'browse, install, publish facets — “npm for hypermedia”',
    variants: [
      { n: 1, title: 'npm-like web', sub: 'search · category · detail card', tag: 'safe', comp: 'RegistryV1' },
      { n: 2, title: 'Visual gallery', sub: 'live previews; install line under each', tag: 'showcase', comp: 'RegistryV2' },
      { n: 3, title: 'CLI-first', sub: 'fct search / view / add / publish', tag: 'power user', comp: 'RegistryV3' },
    ],
  },
  {
    id: 'diagnostics',
    n: 5,
    title: 'Diagnostics',
    intent: 'when fctc fails — how the error lands',
    variants: [
      { n: 1, title: 'Rust-style terminal', sub: 'colored excerpt, caret, help: suggestion', tag: 'CLI', comp: 'DiagnosticsV1' },
      { n: 2, title: 'Problems panel', sub: 'list + source viewer with squiggles', tag: 'IDE-native', comp: 'DiagnosticsV2' },
      { n: 3, title: 'Inline hover-cards', sub: 'errors land in the editor margin', tag: 'inline', comp: 'DiagnosticsV3' },
    ],
  },
];

function App() {
  const [activeId, setActiveId] = React.useState('editor');

  const TWEAK_DEFAULTS = /*EDITMODE-BEGIN*/{
    "roughness": 2,
    "density": "normal",
    "dark": false,
    "annotations": true
  }/*EDITMODE-END*/;

  const [t, setTweak] = window.useTweaks(TWEAK_DEFAULTS);

  // sync body data-* attrs
  React.useEffect(() => {
    document.body.setAttribute('data-rough', String(t.roughness));
    document.body.setAttribute('data-dense', t.density);
    document.body.setAttribute('data-anno', t.annotations ? 'on' : 'off');
    document.body.classList.toggle('dark', !!t.dark);
  }, [t.roughness, t.density, t.annotations, t.dark]);

  const surface = SURFACES.find(s => s.id === activeId);

  return (
    <>
      <div className="topbar">
        <h1>
          <span className="dot">.</span>fct
          <span style={{ fontSize: 17, marginLeft: 12, color: 'var(--muted)', fontFamily: 'Kalam' }}>
            wireframe exploration · {SURFACES.length} surfaces · {SURFACES.reduce((a,s)=>a+s.variants.length,0)} variations
          </span>
        </h1>
        <div className="sub">
          Engineer's notebook — graph paper, mono code, handwritten annotations. Tweak the look in the panel.
        </div>
        <div className="tabs">
          {SURFACES.map(s => (
            <div key={s.id}
              className={'tab ' + (activeId === s.id ? 'active' : '')}
              onClick={() => setActiveId(s.id)}>
              <span className="num">0{s.n}</span>{s.title}
            </div>
          ))}
        </div>
      </div>

      <div className="surface" key={surface.id}>
        <div style={{ width: '100%' }}>
          <SurfaceHeader title={surface.title} intent={surface.intent} n={surface.n} />
        </div>
        {surface.variants.map(v => {
          const Comp = window[v.comp];
          return (
            <div key={v.n} className="variant">
              <VariantHead n={v.n} title={v.title} sub={v.sub} tag={v.tag} />
              {Comp ? <Comp /> : <div className="sbox" style={{padding:20}}>missing: {v.comp}</div>}
              {/* per-variant designer note */}
              <div className="anno" style={{ marginTop: 4 }}>
                {v.n === 1 && surface.id === 'devtools' && 'Familiar to React devs — but the bottom panel is the new thing. Stream IS the runtime.'}
                {v.n === 2 && surface.id === 'devtools' && 'Inverts the usual hierarchy: events are first-class, facets are filters into the event log.'}
                {v.n === 3 && surface.id === 'devtools' && 'Best for teaching the philosophy. Arrows make “signals up, fragments down” physical.'}
                {v.n === 1 && surface.id === 'editor' && 'Lowest-friction onramp. Right inspector is the killer addition — read the contract without reading code.'}
                {v.n === 2 && surface.id === 'editor' && 'Pedagogical. Watching Go template update as you type ".fct" sells the model.'}
                {v.n === 3 && surface.id === 'editor' && 'Most ambitious. Risk: too removed from the source-of-truth file.'}
                {v.n === 1 && surface.id === 'playground' && 'Conventional 3-pane. AST tree pulls double duty as outline.'}
                {v.n === 2 && surface.id === 'playground' && 'Pipeline as the UI. Best for the .fct landing page.'}
                {v.n === 3 && surface.id === 'playground' && 'For when the audience is designers or PMs, not compiler nerds.'}
                {v.n === 1 && surface.id === 'registry' && 'npm-like — fast to comprehend, easy to ship.'}
                {v.n === 2 && surface.id === 'registry' && 'Each facet is visual; previews force authors to make them look good.'}
                {v.n === 3 && surface.id === 'registry' && 'Probably the REAL primary surface. Web is secondary.'}
                {v.n === 1 && surface.id === 'diagnostics' && 'Mimics rustc — gives fct credibility as a real compiler.'}
                {v.n === 2 && surface.id === 'diagnostics' && 'Workspace-level view; LSP needs to emit this format anyway.'}
                {v.n === 3 && surface.id === 'diagnostics' && 'Where authors actually see errors. Hover card carries quick-fix.'}
              </div>
            </div>
          );
        })}
      </div>

      <div className="legend">
        <span><b>roughness</b> — how sketchy the boxes are</span>
        <span><b>annotations</b> — designer’s margin notes (red)</span>
        <span><b>density</b> — padding/gap scale</span>
        <span><b>dark</b> — flips paper → ink</span>
        <span style={{ marginLeft: 'auto', color: 'var(--muted)', fontStyle: 'italic' }}>
          all code = the spec’s action_bar.fct example
        </span>
      </div>

      <window.TweaksPanel title="Tweaks">
        <window.TweakSection label="Aesthetic">
          <window.TweakRadio
            label="Density"
            value={t.density}
            options={[
              { value: 'tight', label: 'tight' },
              { value: 'normal', label: 'normal' },
              { value: 'airy', label: 'airy' },
            ]}
            onChange={(v) => setTweak('density', v)}
          />
          <window.TweakSlider
            label="Roughness"
            value={t.roughness} min={1} max={3} step={1}
            onChange={(v) => setTweak('roughness', v)}
          />
          <window.TweakToggle
            label="Designer annotations"
            value={t.annotations}
            onChange={(v) => setTweak('annotations', v)}
          />
          <window.TweakToggle
            label="Dark paper"
            value={t.dark}
            onChange={(v) => setTweak('dark', v)}
          />
        </window.TweakSection>

        <window.TweakSection label="Jump to surface">
          {SURFACES.map(s => (
            <window.TweakButton
              key={s.id}
              label={`0${s.n} · ${s.title}`}
              onClick={() => setActiveId(s.id)}
            />
          ))}
        </window.TweakSection>
      </window.TweaksPanel>
    </>
  );
}

const root = ReactDOM.createRoot(document.getElementById('root'));
root.render(<App />);
