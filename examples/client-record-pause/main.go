package main

import (
	"log"
	"net"
	"time"

	"github.com/voicecom/gortsplib/v4"
	"github.com/voicecom/gortsplib/v4/pkg/description"
	"github.com/voicecom/gortsplib/v4/pkg/format"
	"github.com/pion/rtp"
)

// This example shows how to
// 1. read H264 frames from a file and generate RTP packets with GStreamer
// 2. connect to a RTSP server, announce an H264 format
// 3. write the frames to the server for 5 seconds
// 4. pause for 5 seconds
// 5. repeat

func main() {
	// open a listener to receive RTP/H264 frames
	pc, err := net.ListenPacket("udp", "localhost:9000")
	if err != nil {
		panic(err)
	}
	defer pc.Close()

	log.Println("Waiting for a RTP/H264 stream on port 9000 - you can send one with GStreamer:\n" +
		"gst-launch-1.0 filesrc location=video.mp4 ! qtdemux ! video/x-h264" +
		" ! h264parse config-interval=1 ! rtph264pay ! udpsink host=127.0.0.1 port=9000")

	// wait for first packet
	buf := make([]byte, 2048)
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		panic(err)
	}
	log.Println("stream connected")

	// create a stream description that contains a H264 format
	desc := &description.Session{
		Medias: []*description.Media{{
			Type: description.MediaTypeVideo,
			Formats: []format.Format{&format.H264{
				PayloadTyp:        96,
				PacketizationMode: 1,
			}},
		}},
	}

	// connect to the server and start recording
	c := gortsplib.Client{}
	err = c.StartRecording("rtsp://localhost:8554/mystream", desc)
	if err != nil {
		panic(err)
	}
	defer c.Close()

	for {
		go func() {
			var pkt rtp.Packet
			for {
				// parse RTP packet
				err = pkt.Unmarshal(buf[:n])
				if err != nil {
					panic(err)
				}

				// route RTP packet to the server
				c.WritePacketRTP(desc.Medias[0], &pkt)

				// read another RTP packet from source
				n, _, err = pc.ReadFrom(buf)
				if err != nil {
					panic(err)
				}
			}
		}()

		// wait
		time.Sleep(5 * time.Second)

		// pause
		_, err := c.Pause()
		if err != nil {
			panic(err)
		}

		// wait
		time.Sleep(5 * time.Second)

		// record again
		_, err = c.Record()
		if err != nil {
			panic(err)
		}
	}
}
