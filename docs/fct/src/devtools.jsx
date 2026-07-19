// Surface 3 — Facet Live DevTools (browser extension panel)
// FA Live: server-authoritative streaming UI. Events flow IN, facets project.

function DevToolsV1() {
  // tree left · inspector right · event log bottom
  const tree = [
    { d: 0, n: '⌂ shell', meta: 'facet:f33d3r:shell', tag: 'static' },
    { d: 1, n: 'feed_container', meta: 'feed:home', tag: 'live' },
    { d: 2, n: 'post_card', meta: 'post:12345', tag: 'live', hi: true },
    { d: 3, n: 'post_avatar', meta: '' },
    { d: 3, n: 'action_bar', meta: '' },
    { d: 4, n: 'like_button', meta: '' },
    { d: 4, n: 'bookmark_button', meta: '' },
    { d: 2, n: 'post_card', meta: 'post:12340', tag: 'live' },
    { d: 1, n: 'notification_badge', meta: 'badge', tag: 'atomic' },
  ];
  return (
    <Frame
      title="🔍 Facet Live DevTools"
      subtitle="feed.home · 4 streams open"
      kind="panel"
      tags={['SSE ●', '328 events/min']}
      style={{ height: 580 }}
    >
      <div style={{ display: 'grid', gridTemplateColumns: '230px 1fr', flex: 1, minHeight: 0 }}>
        {/* tree */}
        <div style={{ borderRight: '1.5px dashed var(--ink)', display: 'flex', flexDirection: 'column', minHeight: 0 }}>
          <div className="mono" style={{ fontSize: 10, color: 'var(--muted)', padding: '6px 10px', borderBottom: '1px dashed var(--ink)' }}>FACET TREE</div>
          <div style={{ padding: '4px 0', overflow: 'auto', flex: 1, fontFamily: 'JetBrains Mono, monospace', fontSize: 11 }}>
            {tree.map((t, i) => (
              <div key={i} style={{
                padding: '2px 10px 2px ' + (10 + t.d * 12) + 'px',
                display: 'flex', alignItems: 'center', gap: 6,
                background: t.hi ? 'var(--hi)' : 'transparent',
                borderLeft: t.hi ? '3px solid var(--accent)' : '3px solid transparent',
              }}>
                <span>{t.n}</span>
                {t.meta && <span style={{ color: 'var(--muted)', fontSize: 10 }}>{t.meta}</span>}
                {t.tag === 'live' && <span className="dot blue" style={{ marginLeft: 'auto' }} />}
                {t.tag === 'atomic' && <span className="dot green" style={{ marginLeft: 'auto' }} />}
                {t.tag === 'static' && <span className="dot muted" style={{ marginLeft: 'auto' }} />}
              </div>
            ))}
          </div>
          <div className="mono" style={{ fontSize: 10, padding: '4px 10px', borderTop: '1px dashed var(--ink)', color: 'var(--muted)', display: 'flex', gap: 8 }}>
            <span><span className="dot muted"></span> static</span>
            <span><span className="dot blue"></span> live</span>
            <span><span className="dot green"></span> atomic</span>
          </div>
        </div>

        {/* inspector + stream */}
        <div style={{ display: 'grid', gridTemplateRows: '1fr 1fr', minHeight: 0 }}>
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', borderBottom: '1.5px dashed var(--ink)', minHeight: 0 }}>
            {/* inputs */}
            <div style={{ padding: 10, borderRight: '1.5px dashed var(--ink)', overflow: 'auto' }}>
              <div className="mono" style={{ fontSize: 10, color: 'var(--muted)' }}>INSPECTOR · post_card</div>
              <div className="scrawl" style={{ fontSize: 18 }}>post:12345</div>
              <div className="mono" style={{ fontSize: 10, color: 'var(--muted)', marginTop: 6 }}>INPUTS (server-resolved)</div>
              <table className="mono" style={{ width: '100%', fontSize: 11, borderCollapse: 'collapse' }}>
                <tbody>
                  {[
                    ['PostID', '"12345"'],
                    ['AuthorName', '"@kate"'],
                    ['BodyHTML', '"&lt;p&gt;…&lt;/p&gt;"'],
                    ['Liked', 'true'],
                    ['LikeCount', '12 ↑'],
                    ['ViewCount', '2,341'],
                    ['CanReply', 'true'],
                  ].map(r => (
                    <tr key={r[0]} style={{ borderBottom: '1px dashed var(--grid)' }}>
                      <td style={{ padding: '2px 4px', color: 'var(--muted)' }}>{r[0]}</td>
                      <td style={{ padding: '2px 4px' }} dangerouslySetInnerHTML={{ __html: r[1] }} />
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            {/* signals + capability */}
            <div style={{ padding: 10, overflow: 'auto' }}>
              <div className="mono" style={{ fontSize: 10, color: 'var(--muted)' }}>SIGNALS</div>
              <div style={{ display: 'flex', flexWrap: 'wrap', gap: 4, marginTop: 4 }}>
                <span className="chip accent" style={{ fontSize: 10 }}>↑ like</span>
                <span className="chip accent" style={{ fontSize: 10 }}>↑ unlike</span>
                <span className="chip accent" style={{ fontSize: 10 }}>↑ bookmark</span>
                <span className="chip blue" style={{ fontSize: 10 }}>↓ like_count_update</span>
                <span className="chip blue" style={{ fontSize: 10 }}>↓ view_count_update</span>
              </div>
              <div className="mono" style={{ fontSize: 10, color: 'var(--muted)', marginTop: 10 }}>CAPABILITY</div>
              <div style={{ display: 'flex', gap: 4, marginTop: 4 }}>
                <span className="chip" style={{ fontSize: 10 }}>listen</span>
                <span className="chip yellow mono" style={{ fontSize: 10 }}>patch</span>
                <span className="chip" style={{ fontSize: 10, opacity: 0.4 }}>emit</span>
                <span className="chip" style={{ fontSize: 10, opacity: 0.4 }}>island</span>
              </div>
              <div className="mono" style={{ fontSize: 10, color: 'var(--muted)', marginTop: 10 }}>STREAMS</div>
              <div className="mono" style={{ fontSize: 11, lineHeight: 1.7 }}>
                <div>● feed.home <span style={{ color: 'var(--muted)' }}>(causal)</span></div>
                <div>● post:12345 <span style={{ color: 'var(--muted)' }}>(mutation)</span></div>
                <div className="anno" style={{ marginTop: 4 }}>last frag: 2.1KB · 38ms ago</div>
              </div>
            </div>
          </div>

          {/* event log */}
          <div style={{ display: 'flex', flexDirection: 'column', minHeight: 0 }}>
            <div className="mono" style={{ fontSize: 10, padding: '4px 10px', borderBottom: '1px dashed var(--ink)', color: 'var(--muted)', display: 'flex', gap: 10 }}>
              <span>SITRA ACHRA · live stream</span>
              <span style={{ color: 'var(--accent)' }}>● recording</span>
              <span style={{ marginLeft: 'auto' }}>filter: post:12345</span>
            </div>
            <div className="mono" style={{ fontSize: 11, overflow: 'auto', flex: 1, padding: 4 }}>
              {[
                ['12:04:31.214', 'facet.replace', 'post:12345.action_bar', 'like_count_update', '218 B'],
                ['12:04:30.802', 'facet.prepend', 'feed:home', 'new_post', '2.1 KB'],
                ['12:04:30.114', 'facet.mutate', 'badge', 'notif_count', '94 B'],
                ['12:04:29.901', 'facet.replace', 'post:12345.view_count', 'view_count_update', '62 B'],
                ['12:04:28.402', 'facet.replace', 'post:12340.action_bar', 'like_count_update', '218 B'],
              ].map((r, i) => (
                <div key={i} style={{
                  display: 'grid', gridTemplateColumns: '90px 110px 1fr 130px 60px',
                  gap: 6, padding: '2px 6px',
                  background: i === 0 ? 'var(--hi)' : 'transparent',
                  borderBottom: '1px dashed var(--grid)',
                }}>
                  <span style={{ color: 'var(--muted)' }}>{r[0]}</span>
                  <span style={{ color: 'var(--accent-2)' }}>{r[1]}</span>
                  <span>{r[2]}</span>
                  <span style={{ color: 'var(--accent)' }}>{r[3]}</span>
                  <span style={{ color: 'var(--muted)' }}>{r[4]}</span>
                </div>
              ))}
            </div>
          </div>
        </div>
      </div>
    </Frame>
  );
}

function DevToolsV2() {
  // Stream-first: the wire IS the application. Big timeline center.
  return (
    <Frame
      title="🛰 Sitra Achra · stream view"
      subtitle="feed.home"
      kind="panel"
      tags={['● live', 'replay ←', '↦ 1.2k events/min']}
      style={{ height: 580 }}
      alt
    >
      <div style={{ display: 'flex', flexDirection: 'column', flex: 1, minHeight: 0 }}>
        {/* filter row */}
        <div style={{ display: 'flex', gap: 6, padding: '6px 10px', borderBottom: '1.5px dashed var(--ink)', flexWrap: 'wrap', alignItems: 'center' }}>
          <span className="mono" style={{ fontSize: 10, color: 'var(--muted)' }}>FILTER ▸</span>
          <span className="chip blue" style={{ fontSize: 11 }}>facet.replace ×</span>
          <span className="chip blue" style={{ fontSize: 11 }}>facet.prepend ×</span>
          <span className="chip" style={{ fontSize: 11, opacity: 0.45 }}>+ facet.remove</span>
          <span className="chip" style={{ fontSize: 11, opacity: 0.45 }}>+ facet.mutate</span>
          <span className="mono" style={{ fontSize: 10, marginLeft: 14, color: 'var(--muted)' }}>PARTITION ▸</span>
          <span className="chip accent" style={{ fontSize: 11 }}>feed.home ×</span>
          <span className="chip" style={{ fontSize: 11, opacity: 0.45 }}>+ post:*</span>
        </div>

        {/* timeline */}
        <div style={{ flex: 1, minHeight: 0, padding: 10, overflow: 'auto', position: 'relative' }}>
          <div style={{ position: 'relative', borderLeft: '2px solid var(--ink)', marginLeft: 70, paddingLeft: 12 }}>
            {[
              { t: '12:04:31', dt: '+0.4s', type: 'replace', target: 'post:12345.action_bar', sig: 'like_count_update', size: '218 B', hi: true,
                frag: '<div class="action-bar" data-state="default">…<span>13</span></div>' },
              { t: '12:04:30', dt: '+0.7s', type: 'prepend', target: 'feed:home', sig: 'new_post', size: '2.1 KB',
                frag: '<article data-fct="post_a1b2" data-facet-id="post:12390">…</article>' },
              { t: '12:04:30', dt: '+0.4s', type: 'mutate', target: 'badge', sig: 'notif_count', size: '94 B',
                frag: '<span data-facet-id="badge">3</span>' },
              { t: '12:04:29', dt: '+1.5s', type: 'replace', target: 'post:12345.view_count', sig: 'view_count_update', size: '62 B',
                frag: '<span>2,341 views</span>' },
              { t: '12:04:28', dt: '+0.8s', type: 'replace', target: 'post:12340.action_bar', sig: 'like_count_update', size: '218 B' },
            ].map((e, i) => (
              <div key={i} style={{ marginBottom: 10, position: 'relative' }}>
                <div style={{
                  position: 'absolute', left: -85, top: 2,
                  fontFamily: 'JetBrains Mono, monospace', fontSize: 10, color: 'var(--muted)',
                  textAlign: 'right', width: 66,
                }}>
                  <div>{e.t}</div>
                  <div style={{ color: 'var(--accent)' }}>{e.dt}</div>
                </div>
                <div style={{
                  position: 'absolute', left: -18, top: 5,
                  width: 10, height: 10, borderRadius: '50%',
                  background: e.hi ? 'var(--accent)' : 'var(--paper)',
                  border: '2px solid var(--ink)',
                }} />
                <div className={`sbox thin ${e.hi ? 'shadow' : ''}`} style={{ padding: '6px 8px', background: e.hi ? 'var(--paper)' : 'var(--paper-2)' }}>
                  <div style={{ display: 'flex', gap: 8, alignItems: 'center', fontFamily: 'JetBrains Mono, monospace', fontSize: 11 }}>
                    <span style={{ color: 'var(--accent-2)' }}>{e.type}</span>
                    <span>→</span>
                    <span><b>{e.target}</b></span>
                    <span style={{ color: 'var(--accent)' }}>· {e.sig}</span>
                    <span style={{ color: 'var(--muted)', marginLeft: 'auto' }}>{e.size}</span>
                  </div>
                  {e.frag && (
                    <div className="mono" style={{ fontSize: 10, color: 'var(--muted)', marginTop: 3, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>
                      fragment ▸ {e.frag}
                    </div>
                  )}
                </div>
              </div>
            ))}
          </div>
          <Pin style={{ top: 14, right: 20 }}>every event = pre-rendered HTML</Pin>
        </div>

        {/* metrics strip */}
        <div style={{ borderTop: '1.5px dashed var(--ink)', padding: '6px 10px', display: 'flex', gap: 16, fontFamily: 'JetBrains Mono, monospace', fontSize: 10 }}>
          <span><span style={{ color: 'var(--muted)' }}>events</span> 1,247</span>
          <span><span style={{ color: 'var(--muted)' }}>bytes</span> 312 KB</span>
          <span><span style={{ color: 'var(--muted)' }}>avg fragment</span> 256 B</span>
          <span><span style={{ color: 'var(--muted)' }}>client cpu</span> &lt; 0.4%</span>
          <span style={{ marginLeft: 'auto' }}><span style={{ color: 'var(--muted)' }}>last_event_id</span> e_2af71</span>
        </div>
      </div>
    </Frame>
  );
}

function DevToolsV3() {
  // Spatial map — facets as cards, signals as arrows
  return (
    <Frame
      title="✸ Facet Map · feed.home"
      subtitle="spatial · live"
      kind="panel"
      tags={['drag = pan', 'scroll = zoom']}
      style={{ height: 580 }}
    >
      <div style={{ position: 'relative', flex: 1, minHeight: 0, overflow: 'hidden', background:
        'radial-gradient(circle at 1px 1px, var(--grid) 1px, transparent 0) 0 0 / 18px 18px',
      }}>
        {/* shell card (background) */}
        <div className="sbox thin dashed" style={{
          position: 'absolute', left: 16, top: 16, right: 16, bottom: 16,
          padding: '4px 8px', background: 'transparent', borderColor: 'var(--muted)',
        }}>
          <div className="mono" style={{ fontSize: 10, color: 'var(--muted)' }}>SHELL · facet:f33d3r:shell  (static)</div>
        </div>

        {/* feed_container */}
        <div className="sbox shadow" style={{ position: 'absolute', left: 50, top: 60, width: 200, padding: 8 }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
            <span className="dot blue"></span>
            <span style={{ fontWeight: 700, fontSize: 13 }}>feed_container</span>
          </div>
          <div className="mono" style={{ fontSize: 10, color: 'var(--muted)' }}>feed:home · composite</div>
          <div className="mono" style={{ fontSize: 10 }}>↓ new_post  ↓ post_removed</div>
        </div>

        {/* post_card 1 (highlighted) */}
        <div className="sbox shadow" style={{
          position: 'absolute', left: 60, top: 200, width: 240, padding: 8,
          borderColor: 'var(--accent)', boxShadow: '3px 3px 0 var(--accent)',
        }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
            <span className="dot"></span>
            <span style={{ fontWeight: 700, fontSize: 13 }}>post_card</span>
            <span className="chip" style={{ marginLeft: 'auto', fontSize: 9 }}>v0.1</span>
          </div>
          <div className="mono" style={{ fontSize: 10, color: 'var(--muted)' }}>post:12345</div>
          <div className="mono" style={{ fontSize: 10, marginTop: 4 }}>children: <span style={{ color: 'var(--accent-2)' }}>action_bar · post_avatar</span></div>
        </div>

        {/* action_bar */}
        <div className="sbox shadow alt" style={{ position: 'absolute', left: 380, top: 230, width: 200, padding: 8 }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
            <span className="dot green"></span>
            <span style={{ fontWeight: 700, fontSize: 13 }}>action_bar</span>
          </div>
          <div className="mono" style={{ fontSize: 10, color: 'var(--muted)' }}>atomic · in post:12345</div>
          <div className="mono" style={{ fontSize: 10, marginTop: 4, color: 'var(--accent)' }}>↑ like ↑ unlike ↑ bookmark</div>
          <div className="mono" style={{ fontSize: 10, color: 'var(--accent-2)' }}>↓ like_count_update</div>
        </div>

        {/* notif badge */}
        <div className="sbox shadow" style={{ position: 'absolute', right: 60, top: 70, width: 170, padding: 8 }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
            <span className="dot green"></span>
            <span style={{ fontWeight: 700, fontSize: 13 }}>notification_badge</span>
          </div>
          <div className="mono" style={{ fontSize: 10, color: 'var(--muted)' }}>badge · atomic</div>
          <div className="mono" style={{ fontSize: 10, marginTop: 4, color: 'var(--accent-2)' }}>↓ notif_count (3)</div>
        </div>

        {/* live event ping */}
        <div className="sbox" style={{
          position: 'absolute', right: 30, bottom: 30, padding: '6px 8px',
          background: 'var(--hi)', borderColor: 'var(--accent)',
        }}>
          <div className="mono" style={{ fontSize: 10 }}>
            <div style={{ color: 'var(--accent)' }}>● 12:04:31</div>
            <div>replace → post:12345.action_bar</div>
            <div style={{ color: 'var(--muted)' }}>fragment 218 B</div>
          </div>
        </div>

        {/* connecting strokes */}
        <svg style={{ position: 'absolute', inset: 0, pointerEvents: 'none' }}>
          {/* feed_container → post_card */}
          <path d="M 150 130 C 150 180 160 180 170 200" stroke="var(--ink)" strokeWidth="1.8" fill="none" strokeDasharray="2 3" />
          {/* post_card → action_bar */}
          <path d="M 300 240 C 340 240 350 250 380 250" stroke="var(--ink)" strokeWidth="1.8" fill="none" />
          {/* action_bar emits */}
          <path d="M 480 280 C 520 320 540 360 540 420" stroke="var(--accent)" strokeWidth="1.6" fill="none" strokeDasharray="3 3" />
          <text x="500" y="370" className="anno" fill="var(--accent)" style={{ fontFamily: 'Caveat', fontSize: 14 }}>POST /events</text>
          {/* event ping → action_bar (replace) */}
          <path d="M 555 480 C 540 380 510 340 480 290" stroke="var(--accent-2)" strokeWidth="1.6" fill="none" />
          <text x="555" y="430" className="anno" fill="var(--accent-2)" style={{ fontFamily: 'Caveat', fontSize: 14 }}>SSE ⇣ fragment</text>
          {/* notif badge → server */}
          <path d="M 680 100 C 720 140 700 180 660 220" stroke="var(--accent-2)" strokeWidth="1.6" fill="none" strokeDasharray="3 3" />
        </svg>

        <Pin style={{ top: 20, right: 24 }}>arrows = live signals</Pin>
        <div className="anno" style={{ position: 'absolute', left: 20, bottom: 14 }}>shell is the antenna — facets are the signal</div>
      </div>
    </Frame>
  );
}

window.DevToolsV1 = DevToolsV1;
window.DevToolsV2 = DevToolsV2;
window.DevToolsV3 = DevToolsV3;
