// Surface 1 — Editor / IDE view of a .fct file
// 3 variations.

function EditorV1() {
  return (
    <Frame
      title="action_bar.fct"
      subtitle="~/f33d3r/web/facets/"
      kind="window"
      tags={['contract ✓', 'layer 2']}
      style={{ height: 560 }}
    >
      <div style={{ display: 'grid', gridTemplateColumns: '150px 1fr 200px', flex: 1, minHeight: 0 }}>
        {/* file tree */}
        <div style={{ borderRight: '1.5px dashed var(--ink)', padding: '10px 8px', fontSize: 13 }}>
          <div className="mono" style={{ color: 'var(--muted)', fontSize: 10, marginBottom: 6 }}>FACETS</div>
          <div style={{ lineHeight: 1.9 }}>
            <div>📁 atoms/</div>
            <div style={{ paddingLeft: 12, color: 'var(--muted)' }}>badge_pill.fct</div>
            <div style={{ paddingLeft: 12, color: 'var(--muted)' }}>post_avatar.fct</div>
            <div>📁 actions/</div>
            <div style={{ paddingLeft: 12, background: 'var(--hi)', borderRadius: 3 }}>● action_bar.fct</div>
            <div style={{ paddingLeft: 12, color: 'var(--muted)' }}>like_button.fct</div>
            <div style={{ paddingLeft: 12, color: 'var(--muted)' }}>bookmark_button.fct</div>
            <div>📁 cards/</div>
            <div style={{ paddingLeft: 12, color: 'var(--muted)' }}>post_card.fct</div>
          </div>
        </div>

        {/* code */}
        <div style={{ display: 'flex', flexDirection: 'column', minHeight: 0 }}>
          <div style={{ display: 'flex', gap: 0, borderBottom: '1px dashed var(--ink)', padding: '4px 8px', fontSize: 11 }} className="mono">
            <span style={{ color: 'var(--muted)' }}>contract</span>
            <span style={{ margin: '0 6px', color: 'var(--muted)' }}>·</span>
            <span style={{ color: 'var(--muted)' }}>template</span>
            <span style={{ margin: '0 6px', color: 'var(--muted)' }}>·</span>
            <span style={{ color: 'var(--accent)' }}>style</span>
          </div>
          <div style={{ flex: 1, minHeight: 0, position: 'relative' }}>
            <CodeBlock height="100%">{FCT_SOURCE_ACTION_BAR}</CodeBlock>
            <div className="anno-arrow" style={{ top: 132, right: 8 }}>
              ← gutter shows<br/>“emits like” here
            </div>
          </div>
        </div>

        {/* contract inspector */}
        <div style={{ borderLeft: '1.5px dashed var(--ink)', padding: '10px 10px', fontSize: 12, display: 'flex', flexDirection: 'column', gap: 10 }}>
          <div>
            <div className="mono" style={{ color: 'var(--muted)', fontSize: 10 }}>CONTRACT</div>
            <div style={{ fontWeight: 700, fontSize: 15 }}>action_bar</div>
            <div className="mono" style={{ color: 'var(--muted)', fontSize: 10 }}>v0.1.0 · layer 2</div>
          </div>
          <div>
            <div className="mono" style={{ color: 'var(--muted)', fontSize: 10, marginBottom: 4 }}>INPUTS · 5</div>
            <div className="mono" style={{ fontSize: 11, lineHeight: 1.55 }}>
              <div>PostID <span style={{ color: 'var(--accent-2)' }}>uuid</span><span style={{ color: 'var(--accent)' }}>!</span></div>
              <div>Liked <span style={{ color: 'var(--accent-2)' }}>bool</span></div>
              <div>LikeCount <span style={{ color: 'var(--accent-2)' }}>int</span></div>
              <div>Book.Count <span style={{ color: 'var(--accent-2)' }}>int</span></div>
              <div>CanReply <span style={{ color: 'var(--accent-2)' }}>bool</span><span style={{ color: 'var(--accent)' }}>!</span></div>
            </div>
          </div>
          <div>
            <div className="mono" style={{ color: 'var(--muted)', fontSize: 10, marginBottom: 4 }}>EMITS · 4</div>
            <div style={{ display: 'flex', flexWrap: 'wrap', gap: 4 }}>
              {['like','unlike','bookmark','unbookmark'].map(e => (
                <span key={e} className="chip accent" style={{ fontSize: 10 }}>{e}</span>
              ))}
            </div>
          </div>
          <div>
            <div className="mono" style={{ color: 'var(--muted)', fontSize: 10, marginBottom: 4 }}>CHILDREN · 2</div>
            <div style={{ display: 'flex', flexWrap: 'wrap', gap: 4 }}>
              <span className="chip blue" style={{ fontSize: 10 }}>like_button</span>
              <span className="chip blue" style={{ fontSize: 10 }}>bookmark_btn</span>
            </div>
          </div>
          <div style={{ marginTop: 'auto' }}>
            <div className="mono" style={{ color: 'var(--muted)', fontSize: 10 }}>BUILD</div>
            <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
              <span className="dot green"></span>
              <span className="mono" style={{ fontSize: 11 }}>go-html ok · 14ms</span>
            </div>
          </div>
        </div>
      </div>

      <div style={{ borderTop: '1.5px dashed var(--ink)', padding: '4px 10px', display: 'flex', gap: 14, fontSize: 11 }} className="mono">
        <span><span className="dot green"></span> validated</span>
        <span style={{ color: 'var(--muted)' }}>ln 14 · col 22</span>
        <span style={{ color: 'var(--muted)' }}>5 inputs · 4 emits · 1 receives · 2 children</span>
        <span style={{ marginLeft: 'auto', color: 'var(--accent-2)' }}>fctc 0.1.0</span>
      </div>
    </Frame>
  );
}

function EditorV2() {
  return (
    <Frame
      title="action_bar.fct  ↔  compiled outputs"
      subtitle="split view"
      kind="window"
      tags={['live']}
      style={{ height: 560 }}
      alt
    >
      <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', flex: 1, minHeight: 0 }}>
        <div style={{ borderRight: '1.5px dashed var(--ink)', display: 'flex', flexDirection: 'column', minHeight: 0 }}>
          <div className="mono" style={{ fontSize: 11, padding: '4px 10px', color: 'var(--muted)', borderBottom: '1px dashed var(--ink)' }}>
            SOURCE · action_bar.fct
          </div>
          <CodeBlock height="100%">{FCT_SOURCE_ACTION_BAR}</CodeBlock>
        </div>

        <div style={{ display: 'flex', flexDirection: 'column', minHeight: 0 }}>
          <div style={{ display: 'flex', gap: 0, borderBottom: '1px dashed var(--ink)' }}>
            {['go template', 'struct.go', 'scoped.css', 'render flow'].map((t, i) => (
              <div key={t}
                className="mono"
                style={{
                  fontSize: 11, padding: '4px 10px',
                  borderRight: i < 3 ? '1px dashed var(--ink)' : 'none',
                  background: i === 0 ? 'var(--hi)' : 'transparent',
                  color: i === 0 ? 'var(--ink)' : 'var(--muted)',
                }}>{t}</div>
            ))}
          </div>
          <div style={{ flex: 1, minHeight: 0, position: 'relative' }}>
            <CodeBlock lang="go" height="100%">{`{{/* generated by fctc · do NOT edit */}}
{{define "action_bar"}}
<div data-fct="ab_a1b2c3"
     class="action-bar"
     data-state="{{.State}}">

  {{template "like_button" (dict
      "PostID" .PostID
      "Liked"  .Liked
      "Count"  .LikeCount )}}

  {{template "bookmark_button" (dict
      "PostID"      .PostID
      "Bookmarked"  .Bookmarked
      "Count"       .BookmarkCount )}}

  <span data-live="view_count_update">
    {{.ViewCount}} views
  </span>
</div>
{{end}}`}</CodeBlock>
            <Pin style={{ top: 14, right: 12 }}>fctc rewrites • selector</Pin>
          </div>
          <div style={{ borderTop: '1.5px dashed var(--ink)', padding: '6px 10px' }}>
            <div className="mono" style={{ fontSize: 10, color: 'var(--muted)' }}>NEXT TARGET</div>
            <div style={{ display: 'flex', gap: 6, marginTop: 4 }}>
              <span className="chip blue">go-html ✓</span>
              <span className="chip" style={{ opacity: 0.55 }}>node-nunjucks</span>
              <span className="chip" style={{ opacity: 0.55 }}>rust-tera</span>
              <span className="chip" style={{ opacity: 0.55 }}>python-jinja</span>
            </div>
          </div>
        </div>
      </div>
    </Frame>
  );
}

function EditorV3() {
  return (
    <Frame
      title="action_bar.fct  —  notebook view"
      subtitle="contract visualised"
      kind="window"
      tags={['layer 2']}
      style={{ height: 560 }}
    >
      <div style={{ padding: 14, overflow: 'auto', flex: 1, minHeight: 0 }}>
        {/* contract as a table */}
        <div className="scrawl" style={{ fontSize: 22, marginBottom: 2 }}>action_bar</div>
        <div className="mono" style={{ fontSize: 11, color: 'var(--muted)', marginBottom: 10 }}>
          v0.1.0 · layer 2 · contract↓
        </div>

        <table className="mono" style={{ width: '100%', fontSize: 11.5, borderCollapse: 'collapse', marginBottom: 14 }}>
          <thead>
            <tr style={{ borderBottom: '1.5px solid var(--ink)' }}>
              <th style={{ textAlign: 'left', padding: '3px 6px' }}>INPUT</th>
              <th style={{ textAlign: 'left', padding: '3px 6px' }}>TYPE</th>
              <th style={{ textAlign: 'left', padding: '3px 6px' }}>REQ</th>
              <th style={{ textAlign: 'left', padding: '3px 6px', color: 'var(--muted)' }}>USED IN</th>
            </tr>
          </thead>
          <tbody>
            {[
              ['PostID', 'uuid', '!', 'template, like_button, bookmark_button'],
              ['Liked', 'bool', '', 'like_button'],
              ['LikeCount', 'int', '', 'like_button'],
              ['BookmarkCount', 'int', '', 'bookmark_button'],
              ['CanReply', 'bool', '!', 'state: disabled'],
            ].map(r => (
              <tr key={r[0]} style={{ borderBottom: '1px dashed var(--grid)' }}>
                <td style={{ padding: '3px 6px' }}>{r[0]}</td>
                <td style={{ padding: '3px 6px', color: 'var(--accent-2)' }}>{r[1]}</td>
                <td style={{ padding: '3px 6px', color: 'var(--accent)' }}>{r[2]}</td>
                <td style={{ padding: '3px 6px', color: 'var(--muted)' }}>{r[3]}</td>
              </tr>
            ))}
          </tbody>
        </table>

        <div style={{ display: 'flex', gap: 14, marginBottom: 10 }}>
          <div className="sbox thin" style={{ flex: 1, padding: 8 }}>
            <div className="mono" style={{ fontSize: 10, color: 'var(--muted)' }}>STATES (first-match)</div>
            <div className="mono" style={{ fontSize: 11.5, marginTop: 4, lineHeight: 1.6 }}>
              <div><span style={{ color: 'var(--accent)' }}>1.</span> disabled <span className="hand" style={{ color: 'var(--accent-2)' }}>when</span> <span className="hand" style={{ color: 'var(--accent-2)' }}>not</span> CanReply</div>
              <div><span style={{ color: 'var(--accent)' }}>2.</span> default</div>
            </div>
          </div>
          <div className="sbox thin" style={{ flex: 1, padding: 8 }}>
            <div className="mono" style={{ fontSize: 10, color: 'var(--muted)' }}>SIGNALS</div>
            <div style={{ display: 'flex', flexWrap: 'wrap', gap: 4, marginTop: 4 }}>
              {['↑ like','↑ unlike','↑ bookmark','↑ unbookmark'].map(s => (
                <span key={s} className="chip accent" style={{ fontSize: 10 }}>{s}</span>
              ))}
              <span className="chip blue" style={{ fontSize: 10 }}>↓ like_count_update</span>
            </div>
          </div>
        </div>

        <div className="mono" style={{ fontSize: 10, color: 'var(--muted)', marginBottom: 4 }}>TEMPLATE</div>
        <div className="sbox thin" style={{ padding: 8, fontFamily: "'JetBrains Mono', monospace", fontSize: 11.5, lineHeight: 1.6 }}>
          <div>&lt;div class="action-bar" data-state="<span style={{color:'var(--accent)'}}>{'{state}'}</span>"&gt;</div>
          <div style={{ paddingLeft: 16 }}>
            <span className="chip blue" style={{ fontSize: 10, marginRight: 6 }}>📦 like_button</span>
            <span style={{ color: 'var(--muted)' }}>:post-id :liked :count @click="like"</span>
          </div>
          <div style={{ paddingLeft: 16 }}>
            <span className="chip blue" style={{ fontSize: 10, marginRight: 6 }}>📦 bookmark_button</span>
            <span style={{ color: 'var(--muted)' }}>:post-id :bookmarked :count @click="bookmark"</span>
          </div>
          <div style={{ paddingLeft: 16 }}>
            &lt;span <span style={{color:'var(--accent-2)'}}>live:text</span>="view_count_update"&gt;
            <span style={{color:'var(--accent)'}}>{'{{ ViewCount }}'}</span> views&lt;/span&gt;
          </div>
          <div>&lt;/div&gt;</div>
        </div>

        <div className="anno" style={{ marginTop: 8 }}>
          ↑ child facets become pills — click to jump into their .fct
        </div>
      </div>
    </Frame>
  );
}

window.EditorV1 = EditorV1;
window.EditorV2 = EditorV2;
window.EditorV3 = EditorV3;
