// Surface 4 — Registry · fct.dev
// Browse, publish, install facets.

function RegistryV1() {
  // npm-style: search, list, detail
  return (
    <Frame
      title="fct.dev"
      subtitle="https://fct.dev"
      kind="window"
      tags={['2,184 facets', 'v0.1']}
      style={{ height: 580 }}
    >
      <div style={{ borderBottom: '1.5px dashed var(--ink)', padding: '8px 14px', display: 'flex', gap: 10, alignItems: 'center' }}>
        <div className="scrawl" style={{ fontWeight: 700, fontSize: 22 }}><span style={{ color: 'var(--accent)' }}>.</span>fct</div>
        <div className="sbox thin" style={{ flex: 1, padding: '4px 10px', display: 'flex', alignItems: 'center', gap: 6, background: 'var(--paper-2)' }}>
          <span className="mono" style={{ color: 'var(--muted)', fontSize: 11 }}>🔍</span>
          <span className="mono" style={{ fontSize: 12 }}>action_bar</span>
          <span className="mono" style={{ marginLeft: 'auto', color: 'var(--muted)', fontSize: 10 }}>⌘K</span>
        </div>
        <span className="chip" style={{ fontSize: 11 }}>publish</span>
        <span className="chip" style={{ fontSize: 11 }}>docs</span>
      </div>

      <div style={{ display: 'grid', gridTemplateColumns: '210px 1fr', flex: 1, minHeight: 0 }}>
        <div style={{ borderRight: '1.5px dashed var(--ink)', padding: 10, fontSize: 12 }}>
          <div className="mono" style={{ fontSize: 10, color: 'var(--muted)' }}>CATEGORY</div>
          <div style={{ lineHeight: 1.9 }}>
            <div>📦 actions <span style={{ color: 'var(--muted)' }}>(38)</span></div>
            <div>📦 atoms <span style={{ color: 'var(--muted)' }}>(214)</span></div>
            <div>📦 cards <span style={{ color: 'var(--muted)' }}>(91)</span></div>
            <div>📦 feeds <span style={{ color: 'var(--muted)' }}>(12)</span></div>
            <div>📦 media <span style={{ color: 'var(--muted)' }}>(46)</span></div>
            <div>📦 nav <span style={{ color: 'var(--muted)' }}>(73)</span></div>
            <div>📦 inputs <span style={{ color: 'var(--muted)' }}>(120)</span></div>
          </div>
          <div className="mono" style={{ fontSize: 10, color: 'var(--muted)', marginTop: 14 }}>TARGET</div>
          <div className="mono" style={{ fontSize: 11, lineHeight: 1.9 }}>
            <div>☑ go-html</div>
            <div>☐ node-nunjucks</div>
            <div>☐ rust-tera</div>
            <div>☐ python-jinja</div>
          </div>
          <div className="mono" style={{ fontSize: 10, color: 'var(--muted)', marginTop: 14 }}>CAPABILITY</div>
          <div className="mono" style={{ fontSize: 11, lineHeight: 1.9 }}>
            <div>☑ zero-js</div>
            <div>☐ listen</div>
            <div>☐ patch</div>
            <div>☐ emit</div>
          </div>
        </div>

        <div style={{ display: 'grid', gridTemplateColumns: '1fr 280px', minHeight: 0 }}>
          <div style={{ overflow: 'auto', padding: 10, borderRight: '1.5px dashed var(--ink)' }}>
            <div className="mono" style={{ fontSize: 10, color: 'var(--muted)', marginBottom: 8 }}>17 RESULTS · SORTED BY DOWNLOADS</div>
            {[
              { name: '@fa/social/action_bar', v: '1.2.0', d: '218k', desc: 'Like / repost / bookmark bar.', hi: true, tags: ['zero-js', 'go-html'] },
              { name: '@kate/action_bar', v: '0.4.1', d: '12k', desc: 'Minimal action bar with optimistic toggles.', tags: ['patch', 'go-html'] },
              { name: '@fa/admin/action_bar', v: '0.9.0', d: '4.1k', desc: 'Admin moderation actions.', tags: ['emit', 'go-html', 'node'] },
              { name: '@retro/action_bar_neon', v: '0.2.3', d: '850', desc: 'CRT-styled, audible feedback.', tags: ['patch'] },
            ].map((p, i) => (
              <div key={i} className={`sbox thin ${p.hi ? 'shadow' : ''}`}
                style={{ padding: 10, marginBottom: 8, background: p.hi ? 'var(--hi)' : 'var(--paper)' }}>
                <div style={{ display: 'flex', alignItems: 'baseline', gap: 8 }}>
                  <span className="mono" style={{ fontWeight: 700, fontSize: 13 }}>{p.name}</span>
                  <span className="mono" style={{ fontSize: 10, color: 'var(--muted)' }}>v{p.v}</span>
                  <span className="mono" style={{ fontSize: 10, color: 'var(--muted)', marginLeft: 'auto' }}>↓ {p.d}/wk</span>
                </div>
                <div style={{ fontSize: 12, marginTop: 2 }}>{p.desc}</div>
                <div style={{ display: 'flex', gap: 4, marginTop: 6 }}>
                  {p.tags.map(t => <span key={t} className="chip" style={{ fontSize: 9 }}>{t}</span>)}
                </div>
              </div>
            ))}
          </div>

          {/* selected detail */}
          <div style={{ padding: 12, overflow: 'auto', minHeight: 0 }}>
            <div className="scrawl" style={{ fontSize: 22, lineHeight: 1 }}>action_bar</div>
            <div className="mono" style={{ fontSize: 10, color: 'var(--muted)' }}>@fa/social · v1.2.0 · MIT</div>

            <div className="sbox thin" style={{ marginTop: 10, padding: 8, background: 'var(--paper-2)' }}>
              <div className="mono" style={{ fontSize: 10, color: 'var(--muted)' }}>INSTALL</div>
              <div className="mono" style={{ fontSize: 11.5, marginTop: 2 }}>$ fct add @fa/social/action_bar</div>
            </div>

            <div className="mono" style={{ fontSize: 10, color: 'var(--muted)', marginTop: 12 }}>CONTRACT</div>
            <div className="mono" style={{ fontSize: 11, lineHeight: 1.6 }}>
              <div>inputs: PostID Liked LikeCount<br/>BookmarkCount CanReply</div>
              <div>emits: like unlike bookmark unbookmark</div>
              <div>children: like_button bookmark_button</div>
            </div>

            <div className="mono" style={{ fontSize: 10, color: 'var(--muted)', marginTop: 12 }}>TARGETS</div>
            <div style={{ display: 'flex', gap: 4, marginTop: 4, flexWrap: 'wrap' }}>
              <span className="chip blue" style={{ fontSize: 10 }}>go-html ✓</span>
              <span className="chip blue" style={{ fontSize: 10 }}>node ✓</span>
              <span className="chip blue" style={{ fontSize: 10 }}>tera ✓</span>
              <span className="chip" style={{ fontSize: 10, opacity: 0.55 }}>jinja —</span>
            </div>

            <div className="mono" style={{ fontSize: 10, color: 'var(--muted)', marginTop: 12 }}>USED BY</div>
            <div style={{ display: 'flex', gap: 4, flexWrap: 'wrap', marginTop: 4 }}>
              <span className="chip" style={{ fontSize: 9 }}>post_card</span>
              <span className="chip" style={{ fontSize: 9 }}>reply_card</span>
              <span className="chip" style={{ fontSize: 9 }}>quote_card</span>
              <span className="chip" style={{ fontSize: 9 }}>+ 14 more</span>
            </div>

            <div className="anno" style={{ marginTop: 12 }}>contract drift detection ↑ alerts on bump</div>
          </div>
        </div>
      </div>
    </Frame>
  );
}

function RegistryV2() {
  // Visual gallery
  const cards = [
    { name: 'action_bar', author: '@fa/social', d: '218k',
      preview: (<div style={{ display: 'flex', gap: 10, fontSize: 12, alignItems: 'center' }}>
        <span style={{ color: 'var(--accent)' }}>♥ 12</span>
        <span>💬 4</span>
        <span>🔖 3</span>
      </div>) },
    { name: 'post_card', author: '@fa/social', d: '156k',
      preview: (<div className="ph" style={{ height: 60 }}>full post layout</div>) },
    { name: 'avatar_pill', author: '@fa/atoms', d: '94k',
      preview: (<div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
        <div style={{ width: 24, height: 24, borderRadius: '50%', border: '1.5px solid var(--ink)' }} />
        <span style={{ fontSize: 12 }}>@kate</span>
      </div>) },
    { name: 'notification_badge', author: '@fa/social', d: '88k',
      preview: (<div style={{ position: 'relative', width: 30, height: 30 }}>
        <div style={{ width: 22, height: 22, border: '1.5px solid var(--ink)', borderRadius: 3 }} />
        <div style={{ position: 'absolute', top: -4, right: -4, background: 'var(--accent)', color: 'white', borderRadius: '50%', width: 18, height: 18, display: 'flex', alignItems: 'center', justifyContent: 'center', fontSize: 10 }}>3</div>
      </div>) },
    { name: 'feed_tab', author: '@fa/social', d: '72k',
      preview: (<div style={{ display: 'flex', gap: 0, fontSize: 11 }}>
        <span style={{ padding: '4px 8px', borderBottom: '2px solid var(--accent)' }}>For you</span>
        <span style={{ padding: '4px 8px', color: 'var(--muted)' }}>Following</span>
      </div>) },
    { name: 'poll_card', author: '@kate', d: '12k',
      preview: (<div style={{ fontSize: 11 }}>
        <div style={{ height: 6, background: 'var(--paper-2)', border: '1px solid var(--ink)', borderRadius: 3, position: 'relative', marginBottom: 3 }}>
          <div style={{ position: 'absolute', inset: 0, width: '68%', background: 'var(--accent)', borderRadius: 3 }} />
        </div>
        <div style={{ height: 6, background: 'var(--paper-2)', border: '1px solid var(--ink)', borderRadius: 3, position: 'relative' }}>
          <div style={{ position: 'absolute', inset: 0, width: '32%', background: 'var(--accent-2)', borderRadius: 3 }} />
        </div>
      </div>) },
  ];

  return (
    <Frame
      title="fct.dev · gallery"
      subtitle="visual browse"
      kind="window"
      tags={['2,184 facets']}
      style={{ height: 580 }}
      alt
    >
      <div style={{ padding: '8px 14px', borderBottom: '1.5px dashed var(--ink)', display: 'flex', gap: 10, alignItems: 'center' }}>
        <span className="scrawl" style={{ fontSize: 18, fontWeight: 700 }}><span style={{color:'var(--accent)'}}>.</span>fct</span>
        <span className="chip yellow" style={{ fontSize: 11 }}>category: actions</span>
        <span className="chip" style={{ fontSize: 11, opacity: 0.55 }}>+ filter</span>
        <span style={{ marginLeft: 'auto' }} className="mono">
          <span style={{ fontSize: 10, color: 'var(--muted)' }}>sort:</span>
          <span style={{ fontSize: 11, marginLeft: 4 }}>downloads ▾</span>
        </span>
      </div>

      <div style={{ flex: 1, minHeight: 0, overflow: 'auto', padding: 12 }}>
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12 }}>
          {cards.map((c, i) => (
            <div key={i} className={`sbox thin ${i === 0 ? 'shadow' : ''}`} style={{ padding: 10, background: 'var(--paper)' }}>
              <div className="mono" style={{ fontSize: 10, color: 'var(--muted)', marginBottom: 4 }}>PREVIEW</div>
              <div className="sbox thin dashed" style={{ padding: 12, minHeight: 70, marginBottom: 8, background: 'var(--paper-2)', display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
                {c.preview}
              </div>
              <div style={{ display: 'flex', alignItems: 'baseline', gap: 6 }}>
                <span style={{ fontWeight: 700, fontSize: 14 }}>{c.name}</span>
                <span className="mono" style={{ fontSize: 10, color: 'var(--muted)' }}>{c.author}</span>
                <span className="mono" style={{ fontSize: 10, color: 'var(--muted)', marginLeft: 'auto' }}>↓ {c.d}</span>
              </div>
              <div className="mono" style={{ fontSize: 10.5, marginTop: 5, background: 'var(--paper-2)', padding: '3px 6px', border: '1px dashed var(--ink)', borderRadius: 3 }}>
                fct add {c.author}/{c.name}
              </div>
            </div>
          ))}
        </div>
      </div>

      <Pin style={{ position: 'absolute', top: 56, right: 24 }}>each card = live preview</Pin>
    </Frame>
  );
}

function RegistryV3() {
  // CLI-forward — the registry is a CLI tool first
  return (
    <Frame
      title="fct — registry CLI"
      subtitle="terminal · ~/f33d3r"
      kind="window"
      tags={['v0.1.0']}
      style={{ height: 580 }}
    >
      <div style={{ background: 'var(--paper-2)', flex: 1, padding: '12px 16px', minHeight: 0, overflow: 'auto', fontFamily: 'JetBrains Mono, monospace', fontSize: 11.5, lineHeight: 1.6 }}>
        <div><span style={{ color: 'var(--accent)' }}>~/f33d3r</span> $ fct search action_bar</div>
        <div style={{ color: 'var(--muted)', marginTop: 2 }}>┌────────────────────────────┬───────┬────────┬─────────────────────┐</div>
        <div>│ name                       │ ver   │ ↓ wk   │ description         │</div>
        <div style={{ color: 'var(--muted)' }}>├────────────────────────────┼───────┼────────┼─────────────────────┤</div>
        <div>│ <span style={{ background: 'var(--hi)' }}>@fa/social/action_bar     </span> │ 1.2.0 │ 218k   │ like/repost/bmark   │</div>
        <div>│ @kate/action_bar           │ 0.4.1 │ 12k    │ optimistic toggles  │</div>
        <div>│ @fa/admin/action_bar       │ 0.9.0 │ 4.1k   │ moderation actions  │</div>
        <div>│ @retro/action_bar_neon     │ 0.2.3 │ 850    │ CRT-styled          │</div>
        <div style={{ color: 'var(--muted)' }}>└────────────────────────────┴───────┴────────┴─────────────────────┘</div>

        <div style={{ marginTop: 10 }}><span style={{ color: 'var(--accent)' }}>~/f33d3r</span> $ fct view @fa/social/action_bar</div>
        <div style={{ color: 'var(--muted)' }}>───────────────────────────────────────────────────────────────────────</div>
        <div><span style={{ color: 'var(--accent-2)' }}>@fa/social/action_bar</span> · <span style={{ color: 'var(--muted)' }}>v1.2.0 · MIT · maintained by fa-team</span></div>
        <div>Like / repost / bookmark bar with optimistic UI.</div>
        <div style={{ marginTop: 4, color: 'var(--muted)' }}>inputs   </div>
        <div>{'  '}PostID: <span style={{ color: 'var(--accent-2)' }}>uuid</span> <span style={{ color: 'var(--accent)' }}>!</span>  Liked: <span style={{ color: 'var(--accent-2)' }}>bool</span>  LikeCount: <span style={{ color: 'var(--accent-2)' }}>int</span></div>
        <div>{'  '}BookmarkCount: <span style={{ color: 'var(--accent-2)' }}>int</span>  CanReply: <span style={{ color: 'var(--accent-2)' }}>bool</span> <span style={{ color: 'var(--accent)' }}>!</span></div>
        <div style={{ color: 'var(--muted)', marginTop: 4 }}>emits    </div>
        <div>{'  '}like unlike bookmark unbookmark</div>
        <div style={{ color: 'var(--muted)', marginTop: 4 }}>targets  </div>
        <div>{'  '}go-html <span style={{ color: '#4a8a44' }}>✓</span>   node-nunjucks <span style={{ color: '#4a8a44' }}>✓</span>   rust-tera <span style={{ color: '#4a8a44' }}>✓</span>   python-jinja <span style={{ color: 'var(--muted)' }}>—</span></div>
        <div style={{ color: 'var(--muted)', marginTop: 4 }}>used by  </div>
        <div>{'  '}post_card reply_card quote_card (+ 14 more)</div>

        <div style={{ marginTop: 10 }}><span style={{ color: 'var(--accent)' }}>~/f33d3r</span> $ fct add @fa/social/action_bar</div>
        <div style={{ color: 'var(--muted)' }}>  ✓ resolved 1 facet, 2 transitive (like_button, bookmark_button)</div>
        <div style={{ color: 'var(--muted)' }}>  ✓ wrote web/facets/vendor/fa-social/action_bar.fct</div>
        <div style={{ color: 'var(--muted)' }}>  ✓ updated facet.lock</div>
        <div>  <span style={{ color: '#4a8a44' }}>installed @fa/social/action_bar@1.2.0</span></div>

        <div style={{ marginTop: 10 }}><span style={{ color: 'var(--accent)' }}>~/f33d3r</span> $ fct publish ./web/facets/post_card.fct</div>
        <div style={{ color: 'var(--muted)' }}>  ▸ validating contract … <span style={{ color: '#4a8a44' }}>ok</span></div>
        <div style={{ color: 'var(--muted)' }}>  ▸ checking semver bump … <span style={{ color: 'var(--accent)' }}>BREAKING: removed input "AuthorURL"</span></div>
        <div style={{ color: 'var(--muted)' }}>  ▸ suggested version: <span style={{ color: 'var(--accent)' }}>2.0.0</span> (was 1.4.2)</div>
        <div>  proceed? [y/N] <span style={{ background: 'var(--ink)', color: 'var(--paper)' }}>_</span></div>
      </div>

      <Pin style={{ position: 'absolute', top: 12, right: 16 }}>contract-aware semver</Pin>
    </Frame>
  );
}

window.RegistryV1 = RegistryV1;
window.RegistryV2 = RegistryV2;
window.RegistryV3 = RegistryV3;
