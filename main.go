package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"radio-bot/server/config"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"
	_ "github.com/joho/godotenv/autoload"
	log "github.com/sirupsen/logrus"
	"layeh.com/gopus"
)

const (
	channels  int = 2
	frameRate int = 48000
	frameSize int = 960
	maxBytes  int = (frameSize * 2) * 2
)

type Connection struct {
	vc        *discordgo.VoiceConnection
	stop      chan struct{}
	streaming bool
}

var (
	connections = make(map[string]*Connection)
	mutex       sync.Mutex
)

func main() {
	settings, err := config.LoadSettings()
	log.SetLevel(log.Level(settings.LogLevel))

	dg, err := discordgo.New("Bot " + settings.DiscordToken)
	if err != nil {
		log.Fatal("Error creating Discord session: ", err)
	}
	log.Debug("Discord session created")

	dg.AddHandler(onMessageCreate)

	err = dg.Open()
	if err != nil {
		log.Fatal("Error opening connection to Discord: ", err)
	}
	defer dg.Close()

	log.Println("Bot is running. Press CTRL+C to exit.")
	select {}
}

func onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {

	if m.Author.ID == s.State.User.ID {
		return
	}

	if strings.HasPrefix(m.Content, "!playradio") {
		args := strings.Fields(m.Content)
		if len(args) < 2 {
			s.ChannelMessageSend(m.ChannelID, "Please specify a radio to play. For example: `!playradio gaucha`")
			return
		}

		radioName := strings.ToLower(args[1])

		streamURLs := map[string]string{
			"gaucha":    "https://liverdgaupoa.rbsdirect.com.br/primary/gaucha_rbs.sdp/playlist.m3u8",
			"atlantida": "https://liverdatlpoa.rbsdirect.com.br/primary/atl_poa.sdp/playlist.m3u8",
		}

		streamURL, ok := streamURLs[radioName]
		if !ok {
			s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Unknown radio station: %s", radioName))
			return
		}

		voiceChannelID := getUserVoiceChannelID(s, m.GuildID, m.Author.ID)
		if voiceChannelID == "" {
			s.ChannelMessageSend(m.ChannelID, "You must be in a voice channel to use this command.")
			return
		}

		mutex.Lock()
		defer mutex.Unlock()

		if conn, ok := connections[m.GuildID]; ok {
			conn.stop <- struct{}{}
			conn.vc.Disconnect()
			delete(connections, m.GuildID)
		}

		vc, err := s.ChannelVoiceJoin(m.GuildID, voiceChannelID, false, true)
		if err != nil {
			log.Println("Error joining voice channel:", err)
			s.ChannelMessageSend(m.ChannelID, "Error joining voice channel.")
			return
		}

		stop := make(chan struct{})
		connections[m.GuildID] = &Connection{
			vc:        vc,
			stop:      stop,
			streaming: true,
		}

		go streamAudio(vc, streamURL, stop)

		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Now playing radio: %s", radioName))
	} else if m.Content == "!stop" {

		mutex.Lock()
		defer mutex.Unlock()

		conn, ok := connections[m.GuildID]
		if !ok || !conn.streaming {
			s.ChannelMessageSend(m.ChannelID, "Nothing is playing.")
			return
		}

		conn.stop <- struct{}{}
		conn.vc.Disconnect()
		delete(connections, m.GuildID)

		s.ChannelMessageSend(m.ChannelID, "Stopped playing.")
	}
}

func getUserVoiceChannelID(s *discordgo.Session, guildID, userID string) string {

	guild, err := s.State.Guild(guildID)
	if err != nil {

		guild, err = s.Guild(guildID)
		if err != nil {
			log.Println("Error getting guild:", err)
			return ""
		}
	}

	for _, vs := range guild.VoiceStates {
		if vs.UserID == userID {
			return vs.ChannelID
		}
	}

	return ""
}

func streamAudio(vc *discordgo.VoiceConnection, streamURL string, stop <-chan struct{}) {
	defer vc.Disconnect()

	log.Println("Starting audio stream...")

	ffmpeg := exec.Command(
		"ffmpeg",
		"-i", streamURL,
		"-f", "s16le",
		"-ar", fmt.Sprint(frameRate),
		"-ac", fmt.Sprint(channels),
		"pipe:1",
	)
	ffmpeg.Stderr = os.Stderr
	ffmpegOut, err := ffmpeg.StdoutPipe()
	if err != nil {
		log.Fatal("Error getting ffmpeg stdout: ", err)
	}

	err = ffmpeg.Start()
	if err != nil {
		log.Fatal("Error starting ffmpeg: ", err)
	}

	buffer := bufio.NewReaderSize(ffmpegOut, 16384)

	opusEncoder, err := gopus.NewEncoder(frameRate, channels, gopus.Audio)
	if err != nil {
		log.Fatal("NewEncoder Error: ", err)
	}

	vc.Speaking(true)
	defer vc.Speaking(false)

	go func() {
		defer ffmpeg.Process.Kill()
		defer vc.Disconnect()

		for {
			select {
			case <-stop:
				log.Println("Stopping stream...")
				return
			default:

				pcm := make([]int16, frameSize*channels)
				err = binary.Read(buffer, binary.LittleEndian, &pcm)
				if err != nil {
					if err == io.EOF {
						log.Println("ffmpeg process ended")
					} else {
						log.Println("Error reading ffmpeg output: ", err)
					}
					return
				}

				opusData, err := opusEncoder.Encode(pcm, frameSize, maxBytes)
				if err != nil {
					log.Println("Error encoding PCM to Opus: ", err)
					return
				}

				if !vc.Ready || vc.OpusSend == nil {
					log.Println("Discord voice connection is not ready")
					return
				}
				vc.OpusSend <- opusData
			}
		}
	}()

	log.Println("Streaming started")

	<-stop
	log.Println("Stream stopped")
}
