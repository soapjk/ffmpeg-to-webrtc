//go:build !js
// +build !js

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/atotto/clipboard"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/h264reader"
)

const (
	h264FrameDuration = time.Millisecond * 33
)

func main() { //nolint
	// Create a new RTCPeerConnection
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				// URLs: []string{"stun:stun.l.google.com:19302"},
				URLs: []string{"stun:47.113.224.5:3478"},
			},
		},
	})
	if err != nil {
		panic(err)
	}
	defer func() {
		if cErr := peerConnection.Close(); cErr != nil {
			fmt.Printf("cannot close peerConnection: %v\n", cErr)
		}
	}()

	iceConnectedCtx, iceConnectedCtxCancel := context.WithCancel(context.Background())

	// Create a video track
	videoTrack, videoTrackErr := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "pion")
	if videoTrackErr != nil {
		panic(videoTrackErr)
	}

	rtpSender, videoTrackErr := peerConnection.AddTrack(videoTrack)
	if videoTrackErr != nil {
		panic(videoTrackErr)
	}

	// Read incoming RTCP packets
	// Before these packets are returned they are processed by interceptors. For things
	// like NACK this needs to be called.
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
				return
			}
		}
	}()

	go func() {
		fmt.Printf("args:%+v\n",os.Args)
		dataPipe, err := RunCommand("ffmpeg", os.Args[1:]...)

		if err != nil {
			panic(err)
		}

		h264, h264Err := h264reader.NewReader(dataPipe)
		if h264Err != nil {
			panic(h264Err)
		}

		// Wait for connection established
		<-iceConnectedCtx.Done()

		// Send our video file frame at a time. Pace our sending so we send it at the same speed it should be played back as.
		// This isn't required since the video is timestamped, but we will such much higher loss if we send all at once.
		//
		// It is important to use a time.Ticker instead of time.Sleep because
		// * avoids accumulating skew, just calling time.Sleep didn't compensate for the time spent parsing the data
		// * works around latency issues with Sleep (see https://github.com/golang/go/issues/44343)
		spsAndPpsCache := []byte{}
		ticker := time.NewTicker(h264FrameDuration)
		for ; true; <-ticker.C {
			nal, h264Err := h264.NextNAL()
			if h264Err == io.EOF {
				fmt.Printf("All video frames parsed and sent")
				os.Exit(0)
			}
			if h264Err != nil {
				panic(h264Err)
			}

			nal.Data = append([]byte{0x00, 0x00, 0x00, 0x01}, nal.Data...)

			if nal.UnitType == h264reader.NalUnitTypeSPS || nal.UnitType == h264reader.NalUnitTypePPS {
				spsAndPpsCache = append(spsAndPpsCache, nal.Data...)
				continue
			} else if nal.UnitType == h264reader.NalUnitTypeCodedSliceIdr {
				nal.Data = append(spsAndPpsCache, nal.Data...)
				spsAndPpsCache = []byte{}
			}

			if h264Err = videoTrack.WriteSample(media.Sample{Data: nal.Data, Duration: time.Second}); h264Err != nil {
				panic(h264Err)
			}
		}
	}()

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("Connection State has changed %s \n", connectionState.String())
		if connectionState == webrtc.ICEConnectionStateConnected {
			iceConnectedCtxCancel()
		}
	})

	// Set the handler for Peer connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		fmt.Printf("Peer Connection State has changed: %s\n", s.String())

		if s == webrtc.PeerConnectionStateFailed {
			// Wait until PeerConnection has had no network activity for 30 seconds or another failure. It may be reconnected using an ICE Restart.
			// Use webrtc.PeerConnectionStateDisconnected if you are interested in detecting faster timeout.
			// Note that the PeerConnection may come back from PeerConnectionStateDisconnected.
			fmt.Println("Peer Connection has gone to failed exiting")
			os.Exit(0)
		}
	})

	// Wait for the offer to be pasted
	offer := webrtc.SessionDescription{}
	Decode(MustReadStdin(), &offer)
	fmt.Printf("read SDP:%+v",offer)
	// Set the remote SessionDescription
	if err = peerConnection.SetRemoteDescription(offer); err != nil {
		panic(err)
	}

	// Create answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	// Sets the LocalDescription, and starts our UDP listeners
	if err = peerConnection.SetLocalDescription(answer); err != nil {
		panic(err)
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	// in a production application you should exchange ICE Candidates via OnICECandidate
	<-gatherComplete

	// Output the answer in base64 so we can paste it in browser
	sdp := Encode(*peerConnection.LocalDescription())
	fmt.Printf("lcoal SDP:%+v",*peerConnection.LocalDescription())
	fmt.Println(sdp)
	clipboard.WriteAll(sdp)

	// Block forever
	select {}
}
