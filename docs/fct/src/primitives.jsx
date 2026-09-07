// Shared sketch primitives, .fct code blocks, layout helpers
// Exported to window so other Babel scripts can use them.

const FCT_SOURCE_ACTION_BAR = `<fct:contract
  name="action_bar"
  version="0.1.0"
  layer="2"
>
  <fct:inputs>
    PostID       : uuid !
    Liked        : bool
    LikeCount    : int
    BookmarkCount: int
    CanReply     : bool !
  </fct:inputs>

  <fct:states>
    disabled when not CanReply
    default
  </fct:states>

  <fct:emits>      like unlike bookmark unbookmark  </fct:emits>
  <fct:receives>   like_count_update                </fct:receives>
  <fct:children>   like_button bookmark_button      </fct:children>
</fct:contract>

<fct:template>
  <div class="action-bar" data-state="{state}">
    <like_button
      :post-id="PostID"
      :liked="Liked"
      :count="LikeCount"
      @click="like" />

    <bookmark_button
      :post-id="PostID"
      :bookmarked="Bookmarked"
      :count="BookmarkCount"
      @click="bookmark" />

    <span live:text="view_count_update">
      {{ ViewCount }} views
    </span>
  </div>
</fct:template>

<fct:style scoped>
  .action-bar { display: flex; gap: 1rem; }
  .action-bar[data-state="disabled"] { opacity: 0.5; }
</fct:style>`;

// ---------- syntax highlighter (very rough) ----------
function highlightFct(src) {
  // protect comments? not needed.
  let out = src
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  // tags
  out = out.replace(/(&lt;\/?)(fct:[a-z]+|[a-z_][a-z0-9_]*)/g,
    (m, b, name) => `${b}<span class="k">${name}</span>`);
  // attributes
  out = out.replace(/(\s)([:@a-z][:@a-z\-]*)(=")/g,
    (m, sp, a, eq) => `${sp}<span class="at">${a}</span>${eq}`);
  // strings
  out = out.replace(/"([^"]*)"/g, '"<span class="s">$1</span>"');
  // types in inputs (string|int|...|uuid|bool|float|any)
  out = out.replace(/(:\s*)(string|int|float|bool|uuid|any)(\b)/g,
    '$1<span class="t">$2</span>$3');
  // {{ }}
  out = out.replace(/\{\{([^}]+)\}\}/g, '<span class="t">{{$1}}</span>');
  // when keyword
  out = out.replace(/(\bwhen\b|\bnot\b|\band\b|\bor\b)/g,
    '<span class="k">$1</span>');
  return out;
}

function highlightGo(src) {
  let out = src.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  out = out.replace(/\b(package|func|type|struct|return|var|const|import)\b/g,
    '<span class="k">$1</span>');
  out = out.replace(/\b(string|int|bool|float64|uuid\.UUID|io\.Writer|error)\b/g,
    '<span class="t">$1</span>');
  out = out.replace(/"([^"]*)"/g, '"<span class="s">$1</span>"');
  out = out.replace(/(\/\/[^\n]*)/g, '<span class="c">$1</span>');
  return out;
}

function highlightCss(src) {
  let out = src.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  out = out.replace(/(\[data-fct="[^"]+"\])/g, '<span class="t">$1</span>');
  out = out.replace(/(\.[a-z][a-z0-9_-]*)/g, '<span class="k">$1</span>');
  out = out.replace(/(:\s*)([^;]+)(;)/g,
    (m, c, v, s) => `${c}<span class="s">${v}</span>${s}`);
  return out;
}

function CodeBlock({ children, lang = 'fct', height, scroll = true }) {
  const src = Array.isArray(children) ? children.join('') : String(children == null ? '' : children);
  const html =
    lang === 'fct' ? highlightFct(src) :
    lang === 'go' ? highlightGo(src) :
    lang === 'css' ? highlightCss(src) :
    src.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  return (
    <pre
      className="code"
      style={{
        margin: 0,
        padding: '10px 12px',
        height,
        overflow: scroll ? 'auto' : 'visible',
        background: 'var(--paper-2)',
        border: '1.5px dashed var(--ink)',
        borderRadius: 4,
      }}
      dangerouslySetInnerHTML={{ __html: html }}
    />
  );
}

// ---------- sketchy frame / window chrome ----------
function Frame({ title, subtitle, children, kind = 'pane', tags = [], style, alt }) {
  return (
    <div className={`sbox shadow ${alt ? 'alt' : ''}`} style={{ display: 'flex', flexDirection: 'column', ...style }}>
      <div style={{
        borderBottom: '1.5px dashed var(--ink)',
        padding: '6px 10px',
        display: 'flex', alignItems: 'center', gap: 8,
        background: 'var(--paper-2)',
        borderTopLeftRadius: 4, borderTopRightRadius: 4,
        fontFamily: "'Kalam', cursive",
        fontSize: 14,
      }}>
        {kind === 'window' && (
          <span style={{ display: 'inline-flex', gap: 4, marginRight: 4 }}>
            <i style={{ width: 8, height: 8, borderRadius: '50%', border: '1.5px solid var(--ink)' }} />
            <i style={{ width: 8, height: 8, borderRadius: '50%', border: '1.5px solid var(--ink)' }} />
            <i style={{ width: 8, height: 8, borderRadius: '50%', border: '1.5px solid var(--ink)' }} />
          </span>
        )}
        {kind === 'panel' && (
          <span className="mono" style={{ color: 'var(--muted)', fontSize: 11 }}>▸</span>
        )}
        <span style={{ fontWeight: 700 }}>{title}</span>
        {subtitle && (
          <span className="mono" style={{ color: 'var(--muted)', fontSize: 11 }}>
            {subtitle}
          </span>
        )}
        <span style={{ marginLeft: 'auto', display: 'flex', gap: 6 }}>
          {tags.map((t, i) => (
            <span key={i} className="chip" style={{ fontSize: 10, padding: '0px 7px' }}>{t}</span>
          ))}
        </span>
      </div>
      <div style={{ flex: 1, display: 'flex', flexDirection: 'column', minHeight: 0 }}>
        {children}
      </div>
    </div>
  );
}

function Pin({ children, style }) {
  return <div className="pin" style={style}>{children}</div>;
}

// Hand-drawn arrow that points from one place to another (rough svg)
function Arrow({ d, label, style }) {
  return (
    <div style={{ position: 'absolute', pointerEvents: 'none', ...style }}>
      <svg width="100" height="60" viewBox="0 0 100 60" style={{ overflow: 'visible' }}>
        <path d={d} fill="none" stroke="var(--accent)" strokeWidth="1.8" strokeLinecap="round" />
        <path d="M 0 0 L 8 3 L 4 -3 Z" fill="var(--accent)"
              style={{ transform: 'translate(95px, 50px) rotate(20deg)' }} />
      </svg>
      {label && <div className="anno" style={{ position: 'absolute', top: -6, left: 30, whiteSpace: 'nowrap' }}>{label}</div>}
    </div>
  );
}

// Section title above each variation row
function VariantHead({ n, title, sub, tag }) {
  return (
    <>
      <div className="variant-head">
        <span className="vnum">V{n}</span>
        <h3>{title}</h3>
        {tag && <span className="vtag">{tag}</span>}
      </div>
      {sub && <div className="variant-sub">{sub}</div>}
    </>
  );
}

function SurfaceHeader({ title, intent, n }) {
  return (
    <div style={{ width: '100%', display: 'flex', alignItems: 'flex-end', gap: 16, marginBottom: 4 }}>
      <div>
        <div className="mono" style={{ color: 'var(--muted)', fontSize: 11, letterSpacing: 1 }}>SURFACE {n} / 5</div>
        <div className="scrawl" style={{ fontSize: 38, lineHeight: 1, fontWeight: 700 }}>{title}</div>
      </div>
      <div className="anno" style={{ paddingBottom: 6, maxWidth: 540 }}>{intent}</div>
    </div>
  );
}

Object.assign(window, {
  FCT_SOURCE_ACTION_BAR,
  CodeBlock, Frame, Pin, Arrow,
  VariantHead, SurfaceHeader,
  highlightFct, highlightGo, highlightCss,
});
