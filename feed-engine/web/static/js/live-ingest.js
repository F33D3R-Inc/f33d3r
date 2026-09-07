// live-ingest.js — WebRTC transport for browser broadcast.
//
// Registers window.F33D3R_LiveIngest, which live-broadcast.js hands a live
// MediaStream from the Capture Substrate. This file carries the stream to the
// media server over WebRTC and does nothing else: it holds no application
// state, renders nothing, and decides nothing. Status, viewer counts and the
// end-of-broadcast decision are server state and arrive as rendered Fragments.
//
// The publish credential is never in this file. The SDP offer goes to a
// same-origin endpoint; the server attaches the credential and forwards it.

(function () {
    'use strict';

    if (window.F33D3R_LiveIngest) return;

    // Video ceiling for the WebRTC leg. The ABR ladder is built server-side from
    // whatever arrives, so this only has to be wide enough not to be the
    // bottleneck for a 4K phone camera.
    var VIDEO_MAX_BITRATE = 20000000;
    // The microphone does not degrade. Opus stereo at 192 kbit/s, matching the
    // audio rate every rung of the server ladder carries.
    var AUDIO_MAX_BITRATE = 192000;

    var pc = null;
    var resourceURL = null;
    var currentStreamID = null;

    // H.264 first in the offer. The browser encodes it in hardware on every
    // current device and it is the one codec the server can pass to viewers
    // without a second encode; VP8 — the browser's own default — forces the
    // server to decode and re-encode every frame. Browsers without codec
    // preference support keep their own order.
    function preferH264(transceiver) {
        if (!transceiver || typeof transceiver.setCodecPreferences !== 'function') return;
        if (!window.RTCRtpSender || typeof RTCRtpSender.getCapabilities !== 'function') return;
        var caps = RTCRtpSender.getCapabilities('video');
        if (!caps || !caps.codecs) return;
        var h264 = [], rest = [];
        caps.codecs.forEach(function (c) {
            (/^video\/h264$/i.test(c.mimeType) ? h264 : rest).push(c);
        });
        if (!h264.length) return;
        try { transceiver.setCodecPreferences(h264.concat(rest)); } catch (e) { /* keep the browser's order */ }
    }

    function log(msg, err) {
        if (err) { console.error('[live-ingest] ' + msg, err); }
        else { console.log('[live-ingest] ' + msg); }
    }

    // The broadcaster surface owns this element. A transport failure happens
    // after start() has already returned, so it cannot reach the caller's own
    // error path — it is reported here instead of failing silently.
    //
    // kind marks what the line is saying: 'transport' for progress that the
    // server's own LIVE badge supersedes, 'fault' for a failure that stands
    // until the broadcaster acts. The stage reads the mark to know which lines
    // it may clear when the server says the broadcast is live.
    function report(msg, kind) {
        var el = document.querySelector('[data-live-bcast-status]');
        if (!el) return;
        el.textContent = msg;
        if (kind) el.setAttribute('data-live-bcast-status-kind', kind);
        else el.removeAttribute('data-live-bcast-status-kind');
    }

    // Opus defaults to mono at a low bitrate. The fmtp line is the only place to
    // ask for stereo and a real bitrate, so it is rewritten in the offer.
    function raiseOpusQuality(sdp) {
        var payload = null;
        var lines = sdp.split(/\r\n|\n/);
        var i;
        for (i = 0; i < lines.length; i++) {
            var m = lines[i].match(/^a=rtpmap:(\d+)\s+opus\/48000\/2/i);
            if (m) { payload = m[1]; break; }
        }
        if (!payload) return sdp;

        var want = 'stereo=1;sprop-stereo=1;maxaveragebitrate=' + AUDIO_MAX_BITRATE +
            ';maxplaybackrate=48000;useinbandfec=1';
        var fmtpPrefix = 'a=fmtp:' + payload + ' ';
        for (i = 0; i < lines.length; i++) {
            if (lines[i].indexOf(fmtpPrefix) === 0) {
                lines[i] = fmtpPrefix + want;
                return lines.join('\r\n');
            }
        }
        // No fmtp line present: add one directly after the rtpmap.
        for (i = 0; i < lines.length; i++) {
            if (lines[i].indexOf('a=rtpmap:' + payload + ' ') === 0) {
                lines.splice(i + 1, 0, fmtpPrefix + want);
                return lines.join('\r\n');
            }
        }
        return sdp;
    }

    // Non-trickle ICE: the whole offer is sent once, complete. WHIP supports
    // trickling over PATCH, but a single complete offer needs no resource
    // round-trips and is what every WHIP server accepts.
    function gatheredOffer(peer) {
        return new Promise(function (resolve) {
            if (peer.iceGatheringState === 'complete') {
                resolve(peer.localDescription);
                return;
            }
            var settled = false;
            function done() {
                if (settled) return;
                settled = true;
                peer.removeEventListener('icegatheringstatechange', check);
                resolve(peer.localDescription);
            }
            function check() {
                if (peer.iceGatheringState === 'complete') done();
            }
            peer.addEventListener('icegatheringstatechange', check);
            // A candidate that never arrives must not hang the broadcast.
            setTimeout(done, 4000);
        });
    }

    function applySenderQuality(peer) {
        peer.getSenders().forEach(function (sender) {
            if (!sender.track || !sender.getParameters) return;
            var params = sender.getParameters();
            if (!params.encodings || !params.encodings.length) params.encodings = [{}];
            if (sender.track.kind === 'video') {
                params.encodings[0].maxBitrate = VIDEO_MAX_BITRATE;
                // Keep the resolution the camera captured; drop frame rate first
                // if the uplink cannot carry it.
                params.degradationPreference = 'maintain-resolution';
            } else {
                params.encodings[0].maxBitrate = AUDIO_MAX_BITRATE;
            }
            if (sender.setParameters) {
                sender.setParameters(params).catch(function (err) {
                    log('could not apply ' + sender.track.kind + ' encoding parameters', err);
                });
            }
        });
    }

    // start returns a promise that always settles: a rejection after the caller
    // has returned cannot be caught by it, so the failure is reported here and
    // the promise resolves rather than becoming an unhandled rejection. The
    // synchronous preconditions below still throw, so a browser that cannot
    // publish at all is caught by the caller's own error path.
    function start(streamID, mediaStream) {
        if (!streamID) throw new Error('no stream id');
        if (!mediaStream) throw new Error('no media stream');
        if (!window.RTCPeerConnection) {
            throw new Error('this browser cannot publish WebRTC');
        }
        if (pc) stop();

        currentStreamID = streamID;
        pc = new RTCPeerConnection({ bundlePolicy: 'max-bundle' });

        mediaStream.getTracks().forEach(function (track) {
            var transceiver = pc.addTransceiver(track, { direction: 'sendonly', streams: [mediaStream] });
            if (track.kind === 'video') preferH264(transceiver);
        });

        // The transport's own state, said on the stage. This is not stream
        // status — that is the server's and arrives as the LIVE badge — it is
        // whether this device's packets are reaching the media server, which
        // only this device can know. It used to go to the console and nowhere
        // else, and a handshake that returned an answer and then failed ICE
        // looked exactly like a broadcast that was working: camera preview
        // running, no message, End button. A failed connection is stopped
        // outright, so the surface never claims a transport it does not have.
        pc.addEventListener('connectionstatechange', function () {
            var state = pc.connectionState;
            log('connection ' + state);
            switch (state) {
                case 'connected':
                    report('Connected to F33D3R. Your broadcast goes live the moment the first segment is packaged.', 'transport');
                    break;
                case 'disconnected':
                    report('Connection to F33D3R interrupted — reconnecting.', 'transport');
                    break;
                case 'failed':
                    stop();
                    report('Camera ready, but the broadcast is not transmitting: the connection to F33D3R failed. ' +
                        'Check that this device can reach the server, then start again.', 'fault');
                    break;
            }
        });

        return pc.createOffer()
            .then(function (offer) {
                offer.sdp = raiseOpusQuality(offer.sdp);
                return pc.setLocalDescription(offer);
            })
            .then(function () { return gatheredOffer(pc); })
            .then(function (local) {
                return fetch('/live/' + encodeURIComponent(streamID) + '/whip', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/sdp' },
                    body: local.sdp,
                    credentials: 'same-origin'
                });
            })
            .then(function (resp) {
                if (!resp.ok) {
                    return resp.text().then(function (body) {
                        throw new Error('ingest refused (' + resp.status + '): ' + body.trim());
                    });
                }
                resourceURL = resp.headers.get('Location') || null;
                return resp.text();
            })
            .then(function (answer) {
                return pc.setRemoteDescription({ type: 'answer', sdp: answer });
            })
            .then(function () {
                applySenderQuality(pc);
                log('publishing ' + streamID);
            })
            .catch(function (err) {
                stop();
                log('publishing failed', err);
                report('Camera ready, but the broadcast is not transmitting: ' +
                    (err && err.message ? err.message : 'ingest error'), 'fault');
            });
    }

    function stop() {
        if (resourceURL) {
            // Best effort teardown; the media server also notices a closed peer.
            fetch(resourceURL, { method: 'DELETE', credentials: 'same-origin' })
                .catch(function (err) { log('teardown request failed', err); });
            resourceURL = null;
        }
        if (pc) {
            try { pc.close(); } catch (err) { log('closing peer connection', err); }
            pc = null;
        }
        currentStreamID = null;
    }

    window.F33D3R_LiveIngest = {
        start: start,
        stop: stop,
        get streamID() { return currentStreamID; }
    };
})();
