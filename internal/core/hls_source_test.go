package core

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aler9/gortsplib"
	"github.com/asticode/go-astits"
	"github.com/gin-gonic/gin"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"

	"github.com/aler9/rtsp-simple-server/internal/h264"
)

type testHLSServer struct {
	s *http.Server
}

func newTestHLSServer() (*testHLSServer, error) {
	ln, err := net.Listen("tcp", "localhost:5780")
	if err != nil {
		return nil, err
	}

	ts := &testHLSServer{}

	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.GET("/stream.m3u8", ts.onPlaylist)
	router.GET("/segment.ts", ts.onSegment)

	ts.s = &http.Server{Handler: router}
	go ts.s.Serve(ln)

	return ts, nil
}

func (ts *testHLSServer) close() {
	ts.s.Shutdown(context.Background())
}

func (ts *testHLSServer) onPlaylist(ctx *gin.Context) {
	cnt := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-ALLOW-CACHE:NO
#EXT-X-TARGETDURATION:2
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:2,
segment.ts
`

	ctx.Writer.Header().Set("Content-Type", `application/x-mpegURL`)
	io.Copy(ctx.Writer, bytes.NewReader([]byte(cnt)))
}

func (ts *testHLSServer) onSegment(ctx *gin.Context) {
	ctx.Writer.Header().Set("Content-Type", `video/MP2T`)
	mux := astits.NewMuxer(context.Background(), ctx.Writer)

	mux.AddElementaryStream(astits.PMTElementaryStream{
		ElementaryPID: 256,
		StreamType:    astits.StreamTypeH264Video,
	})

	mux.SetPCRPID(256)

	mux.WriteTables()

	enc, _ := h264.EncodeAnnexB([][]byte{
		{7, 1, 2, 3}, // SPS
		{8},          // PPS
	})

	mux.WriteData(&astits.MuxerData{
		PID: 256,
		PES: &astits.PESData{
			Header: &astits.PESHeader{
				OptionalHeader: &astits.PESOptionalHeader{
					MarkerBits:      2,
					PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS,
					PTS:             &astits.ClockReference{Base: int64(1 * 90000)},
				},
				StreamID: 224, // = video
			},
			Data: enc,
		},
	})

	ctx.Writer.(http.Flusher).Flush()

	time.Sleep(1 * time.Second)

	enc, _ = h264.EncodeAnnexB([][]byte{
		{5}, // IDR
	})

	mux.WriteData(&astits.MuxerData{
		PID: 256,
		PES: &astits.PESData{
			Header: &astits.PESHeader{
				OptionalHeader: &astits.PESOptionalHeader{
					MarkerBits:      2,
					PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS,
					PTS:             &astits.ClockReference{Base: int64(2 * 90000)},
				},
				StreamID: 224, // = video
			},
			Data: enc,
		},
	})
}

func TestHLSSource(t *testing.T) {
	ts, err := newTestHLSServer()
	require.NoError(t, err)
	defer ts.close()

	p, ok := newInstance("hlsDisable: yes\n" +
		"rtmpDisable: yes\n" +
		"paths:\n" +
		"  proxied:\n" +
		"    source: http://localhost:5780/stream.m3u8\n" +
		"    sourceOnDemand: yes\n")
	require.Equal(t, true, ok)
	defer p.close()

	time.Sleep(1 * time.Second)

	dest, err := gortsplib.DialRead("rtsp://localhost:8554/proxied")
	require.NoError(t, err)

	rtcpRecv := int64(0)
	readDone := make(chan struct{})
	frameRecv := make(chan struct{})
	go func() {
		defer close(readDone)
		dest.ReadFrames(func(trackID int, streamType gortsplib.StreamType, payload []byte) {
			if atomic.SwapInt64(&rtcpRecv, 1) == 0 {
			} else {
				require.Equal(t, gortsplib.StreamTypeRTP, streamType)
				var pkt rtp.Packet
				err := pkt.Unmarshal(payload)
				require.NoError(t, err)
				require.Equal(t, []byte{0x05}, pkt.Payload)
				close(frameRecv)
			}
		})
	}()

	<-frameRecv
	dest.Close()
	<-readDone
}
