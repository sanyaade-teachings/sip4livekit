// Copyright 2024 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sip

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/frostbyte73/core"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/sip/pkg/media"
	"github.com/livekit/sip/pkg/media/dtmf"
	"github.com/livekit/sip/pkg/media/rtp"
	"github.com/livekit/sip/pkg/media/sdp"
	"github.com/livekit/sip/pkg/media/srtp"
	"github.com/livekit/sip/pkg/mixer"
	"github.com/livekit/sip/pkg/stats"
)

type UDPConn interface {
	net.Conn
	ReadFromUDPAddrPort(b []byte) (n int, addr netip.AddrPort, err error)
	WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error)
}

func newUDPConn(conn UDPConn) *udpConn {
	return &udpConn{UDPConn: conn}
}

type udpConn struct {
	UDPConn
	src atomic.Pointer[netip.AddrPort]
	dst atomic.Pointer[netip.AddrPort]
}

func (c *udpConn) GetSrc() (netip.AddrPort, bool) {
	ptr := c.src.Load()
	if ptr == nil {
		return netip.AddrPort{}, false
	}
	addr := *ptr
	return addr, addr.IsValid()
}

func (c *udpConn) SetDst(addr netip.AddrPort) {
	if addr.IsValid() {
		c.dst.Store(&addr)
	}
}

func (c *udpConn) Read(b []byte) (n int, err error) {
	n, addr, err := c.ReadFromUDPAddrPort(b)
	c.src.Store(&addr)
	return n, err
}

func (c *udpConn) Write(b []byte) (n int, err error) {
	dst := c.dst.Load()
	if dst == nil {
		return len(b), nil // ignore
	}
	return c.WriteToUDPAddrPort(b, *dst)
}

type MediaConf struct {
	sdp.MediaConfig
	Processor media.PCM16Processor
}

type MediaConfig struct {
	IP                  netip.Addr
	Ports               rtcconfig.PortRange
	MediaTimeoutInitial time.Duration
	MediaTimeout        time.Duration
}

func NewMediaPort(log logger.Logger, mon *stats.CallMonitor, conf *MediaConfig, sampleRate int) (*MediaPort, error) {
	return NewMediaPortWith(log, mon, nil, conf, sampleRate)
}

func NewMediaPortWith(log logger.Logger, mon *stats.CallMonitor, conn UDPConn, conf *MediaConfig, sampleRate int) (*MediaPort, error) {
	if conn == nil {
		c, err := rtp.ListenUDPPortRange(conf.Ports.Start, conf.Ports.End, netip.AddrFrom4([4]byte{0, 0, 0, 0}))
		if err != nil {
			return nil, err
		}
		conn = c
	}
	mediaTimeout := make(chan struct{})
	p := &MediaPort{
		log:          log,
		mon:          mon,
		externalIP:   conf.IP,
		mediaTimeout: mediaTimeout,
		port:         newUDPConn(conn),
		audioOut:     media.NewSwitchWriter(sampleRate),
		audioIn:      media.NewSwitchWriter(sampleRate),
	}
	p.log.Debugw("listening for media on UDP", "port", p.Port())
	return p, nil
}

// MediaPort combines all functionality related to sending and accepting SIP media.
type MediaPort struct {
	log              logger.Logger
	mon              *stats.CallMonitor
	externalIP       netip.Addr
	port             *udpConn
	mediaTimeout     <-chan struct{}
	mediaReceived    core.Fuse
	dtmfAudioEnabled bool
	closed           atomic.Bool

	mu           sync.Mutex
	conf         *MediaConf
	sess         rtp.Session
	hnd          atomic.Pointer[rtp.Handler]
	dtmfOutRTP   *rtp.Stream
	dtmfOutAudio media.PCM16Writer

	audioOutRTP    *rtp.Stream
	audioOut       *media.SwitchWriter // SIP PCM -> LK RTP
	audioIn        *media.SwitchWriter // LK RTP -> SIP PCM
	audioInHandler rtp.Handler         // for debug only
	dtmfIn         atomic.Pointer[func(ev dtmf.Event)]
}

func (p *MediaPort) EnableTimeout(enabled bool) {
	//p.conn.EnableTimeout(enabled) // FIXME
}

func (p *MediaPort) Close() {
	if !p.closed.CompareAndSwap(false, true) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if w := p.audioOut.Swap(nil); w != nil {
		_ = w.Close()
	}
	if w := p.audioIn.Swap(nil); w != nil {
		_ = w.Close()
	}
	p.audioOutRTP = nil
	p.audioInHandler = nil
	p.dtmfOutRTP = nil
	if p.dtmfOutAudio != nil {
		p.dtmfOutAudio.Close()
		p.dtmfOutAudio = nil
	}
	p.dtmfIn.Store(nil)
	_ = p.port.Close()
}

func (p *MediaPort) Port() int {
	return p.port.LocalAddr().(*net.UDPAddr).Port
}

func (p *MediaPort) Received() <-chan struct{} {
	return p.mediaReceived.Watch()
}

func (p *MediaPort) Timeout() <-chan struct{} {
	return p.mediaTimeout
}

func (p *MediaPort) Config() *MediaConf {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conf
}

// WriteAudioTo sets audio writer that will receive decoded PCM from incoming RTP packets.
func (p *MediaPort) WriteAudioTo(w media.PCM16Writer) {
	if pw := p.audioIn.Swap(w); pw != nil {
		_ = pw.Close()
	}
}

// GetAudioWriter returns audio writer that will send PCM to the destination via RTP.
func (p *MediaPort) GetAudioWriter() media.PCM16Writer {
	return p.audioOut
}

// NewOffer generates an SDP offer for the media.
func (p *MediaPort) NewOffer(encrypted bool) (*sdp.Offer, error) {
	return sdp.NewOffer(p.externalIP, p.Port(), encrypted)
}

// SetAnswer decodes and applies SDP answer for offer from NewOffer. SetConfig must be called with the decoded configuration.
func (p *MediaPort) SetAnswer(offer *sdp.Offer, answerData []byte) (*MediaConf, error) {
	answer, err := sdp.ParseAnswer(answerData)
	if err != nil {
		return nil, err
	}
	mc, err := answer.Apply(offer)
	if err != nil {
		return nil, err
	}
	return &MediaConf{MediaConfig: *mc}, nil
}

// SetOffer decodes the offer from another party and returns encoded answer. To accept the offer, call SetConfig.
func (p *MediaPort) SetOffer(offerData []byte) (*sdp.Answer, *MediaConf, error) {
	offer, err := sdp.ParseOffer(offerData)
	if err != nil {
		return nil, nil, err
	}
	answer, mc, err := offer.Answer(p.externalIP, p.Port())
	if err != nil {
		return nil, nil, err
	}
	return answer, &MediaConf{MediaConfig: *mc}, nil
}

func (p *MediaPort) SetConfig(c *MediaConf) error {
	var crypto string
	if c.Crypto != nil {
		crypto = c.Crypto.Profile.String()
	}
	p.log.Infow("using codecs",
		"audio-codec", c.Audio.Codec.Info().SDPName, "audio-rtp", c.Audio.Type,
		"dtmf-rtp", c.Audio.DTMFType,
		"srtp", crypto,
	)

	p.port.SetDst(c.Remote)
	var (
		sess rtp.Session
		err  error
	)
	if c.Crypto != nil {
		sess, err = srtp.NewSession(p.port, c.Crypto)
	} else {
		sess = rtp.NewSession(p.port)
	}
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.port.SetDst(c.Remote)
	p.conf = c
	p.sess = sess

	if err = p.setupOutput(); err != nil {
		return err
	}
	p.setupInput()
	return nil
}

func (p *MediaPort) rtpLoop(sess rtp.Session) {
	// Need a loop to process all incoming packets.
	first := true
	for {
		r, ssrc, err := sess.AcceptStream()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				p.log.Errorw("cannot accept RTP stream", err)
			}
			return
		}
		p.mediaReceived.Break()
		if first {
			first = false
			p.log.Infow("accepting media", "ssrc", ssrc)
			go p.rtpReadLoop(r)
		} else {
			p.log.Warnw("ignoring media", nil, "ssrc", ssrc)
		}
	}
}

func (p *MediaPort) rtpReadLoop(r rtp.ReadStream) {
	buf := make([]byte, 1500)
	var h rtp.Header
	for {
		h = rtp.Header{}
		n, err := r.ReadRTP(&h, buf)
		if err == io.EOF {
			return
		} else if err != nil {
			p.log.Errorw("read RTP failed", err)
			return
		}

		ptr := p.hnd.Load()
		if ptr == nil {
			continue
		}
		hnd := *ptr
		if hnd == nil {
			continue
		}
		err = hnd.HandleRTP(&h, buf[:n])
		if err != nil {
			p.log.Errorw("handle RTP failed", err)
			continue
		}
	}
}

// Must be called holding the lock
func (p *MediaPort) setupOutput() error {
	go p.rtpLoop(p.sess)
	w, err := p.sess.OpenWriteStream()
	if err != nil {
		return err
	}

	// TODO: this says "audio", but actually includes DTMF too
	s := rtp.NewSeqWriter(newRTPStatsWriter(p.mon, "audio", w))
	p.audioOutRTP = s.NewStream(p.conf.Audio.Type, p.conf.Audio.Codec.Info().RTPClockRate)

	// Encoding pipeline (LK -> SIP)
	audioOut := p.conf.Audio.Codec.EncodeRTP(p.audioOutRTP)
	if processor := p.conf.Processor; processor != nil {
		audioOut = processor(audioOut)
	}

	if p.conf.Audio.DTMFType != 0 {
		p.dtmfOutRTP = s.NewStream(p.conf.Audio.DTMFType, dtmf.SampleRate)
		if p.dtmfAudioEnabled {
			// Add separate mixer for DTMF audio.
			// TODO: optimize, if we'll ever need this code path
			mix := mixer.NewMixer(audioOut, rtp.DefFrameDur)
			audioOut = mix.NewInput()
			p.dtmfOutAudio = mix.NewInput()
		}
	}

	if w := p.audioOut.Swap(audioOut); w != nil {
		_ = w.Close()
	}
	return nil
}

func (p *MediaPort) setupInput() {
	// Decoding pipeline (SIP -> LK)
	audioHandler := p.conf.Audio.Codec.DecodeRTP(p.audioIn, p.conf.Audio.Type)
	p.audioInHandler = audioHandler
	audioHandler = rtp.HandleJitter(p.conf.Audio.Codec.Info().RTPClockRate, audioHandler)
	mux := rtp.NewMux(nil)
	mux.SetDefault(newRTPStatsHandler(p.mon, "", nil))
	mux.Register(p.conf.Audio.Type, newRTPStatsHandler(p.mon, p.conf.Audio.Codec.Info().SDPName, audioHandler))
	if p.conf.Audio.DTMFType != 0 {
		mux.Register(p.conf.Audio.DTMFType, newRTPStatsHandler(p.mon, dtmf.SDPName, rtp.HandlerFunc(func(h *rtp.Header, payload []byte) error {
			ptr := p.dtmfIn.Load()
			if ptr == nil {
				return nil
			}
			fnc := *ptr
			if ev, ok := dtmf.DecodeRTP(h, payload); ok && fnc != nil {
				fnc(ev)
			}
			return nil
		})))
	}
	var hnd rtp.Handler = mux
	p.hnd.Store(&hnd)
}

// SetDTMFAudio forces SIP to generate audio dTMF tones in addition to digital signals.
func (p *MediaPort) SetDTMFAudio(enabled bool) {
	p.dtmfAudioEnabled = enabled
}

// HandleDTMF sets an incoming DTMF handler.
func (p *MediaPort) HandleDTMF(h func(ev dtmf.Event)) {
	if h == nil {
		p.dtmfIn.Store(nil)
	} else {
		p.dtmfIn.Store(&h)
	}
}

func (p *MediaPort) WriteDTMF(ctx context.Context, digits string) error {
	if len(digits) == 0 {
		return nil
	}
	p.mu.Lock()
	dtmfOut := p.dtmfOutRTP
	audioOut := p.dtmfOutAudio
	audioOutRTP := p.audioOutRTP
	p.mu.Unlock()
	if !p.dtmfAudioEnabled {
		audioOut = nil
	}
	if dtmfOut == nil && audioOut == nil {
		return nil
	}

	var rtpTs uint32
	if audioOutRTP != nil {
		rtpTs = audioOutRTP.GetCurrentTimestamp()
	}

	return dtmf.Write(ctx, audioOut, dtmfOut, rtpTs, digits)
}
