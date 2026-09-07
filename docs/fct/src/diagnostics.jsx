// Surface 5 — Compiler diagnostics

function DiagnosticsV1() {
  // Rust-style terminal diagnostics
  return (
    <Frame
      title="fctc build · 3 errors, 2 warnings"
      subtitle="~/f33d3r $"
      kind="window"
      tags={['exit 1']}
      style={{ height: 580 }}
    >
      <div style={{ background: 'var(--paper-2)', flex: 1, padding: '14px 18px', minHeight: 0, overflow: 'auto', fontFamily: 'JetBrains Mono, monospace', fontSize: 11.5, lineHeight: 1.55 }}>
        <div>$ fctc build --target go-html --src web/facets/</div>
        <div style={{ color: 'var(--muted)' }}>  ▸ parsing 38 files…</div>
        <div style={{ color: 'var(--muted)' }}>  ▸ resolving facet graph…</div>
        <div style={{ color: 'var(--muted)' }}>  ▸ contract check…</div>
        <br/>

        <div><span style={{ color: 'var(--accent)', fontWeight: 700 }}>error[FC0231]</span>: input <span style={{ color: 'var(--accent)' }}>`ViewCount`</span> used in template but not declared in contract</div>
        <div style={{ color: 'var(--accent-2)' }}>  ┌─ web/facets/actions/action_bar.fct:24:11</div>
        <div style={{ color: 'var(--accent-2)' }}>  │</div>
        <div style={{ color: 'var(--accent-2)' }}>22 │</div>
        <div style={{ color: 'var(--accent-2)' }}>23 │   <span style={{ color: 'var(--ink)' }}>&lt;span live:text="view_count_update"&gt;</span></div>
        <div style={{ color: 'var(--accent-2)' }}>24 │     <span style={{ color: 'var(--ink)' }}>{'{{ ViewCount }}'} views</span></div>
        <div style={{ color: 'var(--accent-2)' }}>   │     <span style={{ color: 'var(--accent)' }}>^^^^^^^^^^^^^^^ undeclared input</span></div>
        <div style={{ color: 'var(--accent-2)' }}>25 │   <span style={{ color: 'var(--ink)' }}>&lt;/span&gt;</span></div>
        <div style={{ color: 'var(--accent-2)' }}>   │</div>
        <div style={{ color: 'var(--accent-2)' }}>   = <span style={{ color: 'var(--ink)' }}>help: add the input to the contract</span></div>
        <div style={{ color: 'var(--accent-2)' }}>     <span style={{ color: 'var(--ink)' }}>{'      <fct:inputs>'}</span></div>
        <div style={{ color: 'var(--accent-2)' }}>     <span style={{ color: '#4a8a44' }}>     +  ViewCount : int</span></div>
        <div style={{ color: 'var(--accent-2)' }}>     <span style={{ color: 'var(--ink)' }}>{'       </fct:inputs>'}</span></div>

        <br/>

        <div><span style={{ color: 'var(--accent)', fontWeight: 700 }}>error[FC0114]</span>: child facet <span style={{ color: 'var(--accent)' }}>`bookmark_button`</span> requires input <span style={{ color: 'var(--accent)' }}>`Bookmarked`</span> but parent did not provide it</div>
        <div style={{ color: 'var(--accent-2)' }}>  ┌─ web/facets/actions/action_bar.fct:17:5</div>
        <div style={{ color: 'var(--accent-2)' }}>  │</div>
        <div style={{ color: 'var(--accent-2)' }}>17 │     <span style={{ color: 'var(--ink)' }}>&lt;bookmark_button</span></div>
        <div style={{ color: 'var(--accent-2)' }}>   │     <span style={{ color: 'var(--accent)' }}>^^^^^^^^^^^^^^^^</span></div>
        <div style={{ color: 'var(--accent-2)' }}>18 │       <span style={{ color: 'var(--ink)' }}>:post-id="PostID"</span></div>
        <div style={{ color: 'var(--accent-2)' }}>19 │       <span style={{ color: 'var(--ink)' }}>:count="BookmarkCount" /&gt;</span></div>
        <div style={{ color: 'var(--accent-2)' }}>   │</div>
        <div style={{ color: 'var(--accent-2)' }}>   = <span style={{ color: 'var(--ink)' }}>note: bookmark_button declares </span><span style={{ color: 'var(--accent)' }}>Bookmarked: bool !</span></div>
        <div style={{ color: 'var(--accent-2)' }}>   = <span style={{ color: 'var(--ink)' }}>help: pass </span><span style={{ color: '#4a8a44' }}>:bookmarked="Bookmarked"</span></div>

        <br/>

        <div><span style={{ color: 'var(--accent-3)', fontWeight: 700 }}>warning[FC0301]</span>: signal <span style={{ color: 'var(--accent-3)' }}>`unbookmark`</span> declared in emits but never used</div>
        <div style={{ color: 'var(--accent-2)' }}>  ┌─ web/facets/actions/action_bar.fct:8:42</div>
        <div style={{ color: 'var(--accent-2)' }}>  │</div>
        <div style={{ color: 'var(--accent-2)' }}>8 │ <span style={{ color: 'var(--ink)' }}>  emits like unlike bookmark unbookmark</span></div>
        <div style={{ color: 'var(--accent-2)' }}>  │ <span style={{ color: 'var(--accent-3)' }}>                                ^^^^^^^^^^</span></div>

        <br/>
        <div><span style={{ color: 'var(--accent)' }}>error</span>: could not compile <span style={{ color: 'var(--accent-2)' }}>action_bar.fct</span> due to 2 previous errors; 1 warning emitted</div>
        <br/>
        <div style={{ color: 'var(--muted)' }}>For more information about this error, try `fctc explain FC0231`.</div>
      </div>
    </Frame>
  );
}

function DiagnosticsV2() {
  // GUI: problem list left, source right with squiggles
  const problems = [
    { kind: 'err', code: 'FC0231', file: 'action_bar.fct', loc: '24:11', msg: '`ViewCount` undeclared', hi: true },
    { kind: 'err', code: 'FC0114', file: 'action_bar.fct', loc: '17:5', msg: '`bookmark_button` missing input `Bookmarked`' },
    { kind: 'warn', code: 'FC0301', file: 'action_bar.fct', loc: '8:42', msg: 'unused signal `unbookmark`' },
    { kind: 'warn', code: 'FC0302', file: 'post_card.fct', loc: '32:9', msg: 'state `loading` unreachable' },
    { kind: 'err', code: 'FC0440', file: 'feed_tab.fct', loc: '12:4', msg: 'duplicate input name `Active`' },
  ];

  return (
    <Frame
      title="Problems · 3 errors · 2 warnings"
      subtitle="fctc build"
      kind="panel"
      tags={['workspace']}
      style={{ height: 580 }}
      alt
    >
      <div style={{ display: 'grid', gridTemplateColumns: '280px 1fr', flex: 1, minHeight: 0 }}>
        <div style={{ borderRight: '1.5px dashed var(--ink)', overflow: 'auto', minHeight: 0 }}>
          {problems.map((p, i) => (
            <div key={i} style={{
              padding: '8px 10px',
              borderBottom: '1px dashed var(--grid)',
              background: p.hi ? 'var(--hi)' : 'transparent',
              borderLeft: p.hi ? '3px solid var(--accent)' : '3px solid transparent',
              display: 'flex', flexDirection: 'column', gap: 2,
            }}>
              <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
                <span style={{
                  fontSize: 9, fontFamily: 'JetBrains Mono, monospace',
                  padding: '1px 5px', borderRadius: 3,
                  background: p.kind === 'err' ? 'var(--accent)' : 'var(--accent-3)',
                  color: 'white', fontWeight: 700,
                }}>{p.kind === 'err' ? 'ERR' : 'WARN'}</span>
                <span className="mono" style={{ fontSize: 10, color: 'var(--muted)' }}>{p.code}</span>
              </div>
              <div style={{ fontSize: 12 }}>{p.msg}</div>
              <div className="mono" style={{ fontSize: 10, color: 'var(--muted)' }}>{p.file} · {p.loc}</div>
            </div>
          ))}
        </div>

        <div style={{ display: 'flex', flexDirection: 'column', minHeight: 0 }}>
          <div className="mono" style={{ fontSize: 11, padding: '4px 10px', borderBottom: '1px dashed var(--ink)', color: 'var(--muted)' }}>
            web/facets/actions/action_bar.fct
          </div>
          <div style={{ position: 'relative', flex: 1, minHeight: 0, overflow: 'auto', padding: '8px 10px', fontFamily: 'JetBrains Mono, monospace', fontSize: 11.5, lineHeight: 1.7 }}>
            {[
              '<fct:contract name="action_bar" version="0.1.0">',
              '  <fct:inputs>',
              '    PostID       : uuid !',
              '    Liked        : bool',
              '    LikeCount    : int',
              '    BookmarkCount: int',
              '    CanReply     : bool !',
              '  </fct:inputs>',
              '  <fct:emits>like unlike bookmark unbookmark</fct:emits>',
              '  <fct:children>like_button bookmark_button</fct:children>',
              '</fct:contract>',
              '',
              '<fct:template>',
              '  <div class="action-bar" data-state="{state}">',
              '    <like_button',
              '      :post-id="PostID" :liked="Liked" :count="LikeCount" />',
              '',
              '    <bookmark_button',
              '      :post-id="PostID"',
              '      :count="BookmarkCount" />',
              '',
              '    <span live:text="view_count_update">',
              '      {{ ViewCount }} views',
              '    </span>',
              '  </div>',
              '</fct:template>',
            ].map((line, i) => {
              const lineNo = i + 1;
              const errLine = lineNo === 23 || lineNo === 17;
              const warnLine = lineNo === 8;
              return (
                <div key={i} style={{ display: 'flex', gap: 10, background: errLine ? 'rgba(200,67,30,0.08)' : warnLine ? 'rgba(184,138,0,0.10)' : 'transparent' }}>
                  <span style={{ color: 'var(--muted)', width: 22, textAlign: 'right', flexShrink: 0 }}>{lineNo}</span>
                  <span style={{ flex: 1, whiteSpace: 'pre' }}>
                    {lineNo === 23 ? (
                      <>{'      '}<span className="squiggle">{'{{ ViewCount }}'}</span>{' views'}</>
                    ) : lineNo === 17 ? (
                      <><span className="squiggle">{'    <bookmark_button'}</span></>
                    ) : lineNo === 8 ? (
                      <>{'  <fct:emits>like unlike bookmark '}<span style={{ textDecoration: 'underline wavy var(--accent-3)' }}>unbookmark</span>{'</fct:emits>'}</>
                    ) : line}
                  </span>
                  {lineNo === 23 && <span className="anno" style={{ fontSize: 12 }}>← FC0231 add to {'<fct:inputs>'}</span>}
                  {lineNo === 17 && <span className="anno" style={{ fontSize: 12 }}>← FC0114 missing :bookmarked=</span>}
                </div>
              );
            })}
          </div>
        </div>
      </div>
    </Frame>
  );
}

function DiagnosticsV3() {
  // Inline editor errors with rich hover card
  return (
    <Frame
      title="action_bar.fct  —  with diagnostics"
      subtitle="inline"
      kind="window"
      tags={['hover errors']}
      style={{ height: 580 }}
    >
      <div style={{ position: 'relative', flex: 1, minHeight: 0, overflow: 'auto', padding: '10px 0', fontFamily: 'JetBrains Mono, monospace', fontSize: 12, lineHeight: 1.7 }}>
        {[
          { n: 1, c: '<fct:contract name="action_bar" version="0.1.0">' },
          { n: 2, c: '  <fct:inputs>' },
          { n: 3, c: '    PostID       : uuid !' },
          { n: 4, c: '    Liked        : bool' },
          { n: 5, c: '    LikeCount    : int' },
          { n: 6, c: '    BookmarkCount: int' },
          { n: 7, c: '    CanReply     : bool !' },
          { n: 8, c: '  </fct:inputs>' },
          { n: 9, c: '  <fct:emits>', warn: true,
            full: '  <fct:emits>like unlike bookmark unbookmark</fct:emits>' },
          { n: 10, c: '  <fct:children>like_button bookmark_button</fct:children>' },
          { n: 11, c: '</fct:contract>' },
          { n: 12, c: '' },
          { n: 13, c: '<fct:template>' },
          { n: 14, c: '  <div class="action-bar" data-state="{state}">' },
          { n: 15, c: '    <like_button' },
          { n: 16, c: '      :post-id="PostID" :liked="Liked" :count="LikeCount" />' },
          { n: 17, c: '    <bookmark_button', err: true, msg: 'missing required input `Bookmarked: bool !`' },
          { n: 18, c: '      :post-id="PostID"' },
          { n: 19, c: '      :count="BookmarkCount" />' },
          { n: 20, c: '' },
          { n: 21, c: '    <span live:text="view_count_update">' },
          { n: 22, c: '      {{ ViewCount }} views', err: true, msg: 'input `ViewCount` not declared in contract',
            hover: true },
          { n: 23, c: '    </span>' },
          { n: 24, c: '  </div>' },
          { n: 25, c: '</fct:template>' },
        ].map((l, i) => (
          <div key={i} style={{ display: 'flex', gap: 8, alignItems: 'flex-start', position: 'relative', padding: '0 14px' }}>
            <span style={{ color: 'var(--muted)', width: 24, textAlign: 'right', fontSize: 11 }}>{l.n}</span>
            <span style={{
              width: 14, color: l.err ? 'var(--accent)' : l.warn ? 'var(--accent-3)' : 'transparent',
              fontWeight: 700,
            }}>
              {l.err ? '✘' : l.warn ? '⚠' : ''}
            </span>
            <span style={{ flex: 1, whiteSpace: 'pre',
              background: l.err ? 'rgba(200,67,30,0.08)' :
                          l.warn ? 'rgba(184,138,0,0.10)' : 'transparent',
              borderRadius: 3, padding: '0 4px',
            }}>
              {l.warn ?
                <>{'  <fct:emits>like unlike bookmark '}<span style={{ textDecoration: 'underline wavy var(--accent-3) 2px' }}>unbookmark</span>{'</fct:emits>'}</>
                : l.full ? l.full
                : l.err && l.n === 22 ? <>{'      '}<span style={{ textDecoration: 'underline wavy var(--accent) 2px' }}>{'{{ ViewCount }}'}</span>{' views'}</>
                : l.err && l.n === 17 ? <span style={{ textDecoration: 'underline wavy var(--accent) 2px' }}>{l.c}</span>
                : l.c}
            </span>
            {l.msg && !l.hover && (
              <span className="anno" style={{ fontSize: 12, color: l.err ? 'var(--accent)' : 'var(--accent-3)', whiteSpace: 'nowrap' }}>
                ← {l.msg}
              </span>
            )}
            {l.hover && (
              <div className="sbox shadow" style={{
                position: 'absolute', left: 200, top: 22, zIndex: 5,
                padding: 10, width: 320,
                background: 'var(--paper)', borderColor: 'var(--accent)',
              }}>
                <div style={{ display: 'flex', gap: 6, alignItems: 'center', marginBottom: 4 }}>
                  <span style={{
                    fontSize: 9, fontFamily: 'JetBrains Mono, monospace',
                    padding: '1px 5px', borderRadius: 3,
                    background: 'var(--accent)', color: 'white', fontWeight: 700,
                  }}>ERR</span>
                  <span className="mono" style={{ fontSize: 10, color: 'var(--muted)' }}>FC0231</span>
                </div>
                <div style={{ fontFamily: 'Kalam', fontSize: 13, lineHeight: 1.35 }}>
                  Input <span className="mono" style={{ color: 'var(--accent)' }}>ViewCount</span> is used in the template but is not declared in <span className="mono">{'<fct:inputs>'}</span>.
                </div>
                <div className="mono" style={{ marginTop: 8, fontSize: 11, background: 'var(--paper-2)', padding: 6, border: '1px dashed var(--ink)', borderRadius: 3 }}>
                  <div style={{ color: 'var(--muted)' }}># suggested fix</div>
                  <div><span style={{ color: '#4a8a44' }}>+   ViewCount : int</span></div>
                </div>
                <div className="anno" style={{ marginTop: 6, fontSize: 12 }}>quick-fix: ⌥↵</div>
              </div>
            )}
          </div>
        ))}
      </div>

      <div style={{ borderTop: '1.5px dashed var(--ink)', padding: '4px 12px', display: 'flex', gap: 14, fontSize: 11 }} className="mono">
        <span style={{ color: 'var(--accent)' }}>✘ 2 errors</span>
        <span style={{ color: 'var(--accent-3)' }}>⚠ 1 warning</span>
        <span style={{ color: 'var(--muted)' }}>ln 22 · col 9</span>
        <span style={{ marginLeft: 'auto', color: 'var(--muted)' }}>fctc 0.1.0</span>
      </div>
    </Frame>
  );
}

window.DiagnosticsV1 = DiagnosticsV1;
window.DiagnosticsV2 = DiagnosticsV2;
window.DiagnosticsV3 = DiagnosticsV3;
