package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"radio-bot/server/config"
	"strconv"
	"strings"
	"sync"
	"time"

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

type RadioStation struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type Connection struct {
	vc        *discordgo.VoiceConnection
	stop      chan struct{}
	done      chan struct{}
	streaming bool
	volume    float64
	volumeMu  sync.RWMutex
	paused    bool
	pauseMu   sync.Mutex
}

var (
	connections = make(map[string]*Connection)
	mutex       sync.Mutex

	streamURLs = map[string]string{}

	customRadios      = make(map[string]string)
	customRadiosMutex sync.RWMutex

	searchResults      = make(map[string][]RadioStation)
	searchResultsMutex sync.Mutex
)

func main() {
	settings, err := config.LoadSettings()
	if err != nil {
		log.Fatal("Error loading settings: ", err)
	}
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

	loadCustomRadios()

	log.Println("Bot is running. Press CTRL+C to exit.")
	select {}
}

func onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {

	if m.Author.ID == s.State.User.ID {
		return
	}

	if m.Content == "!help" {
		helpMessage := "**Available Commands:**\n" +
			"- `!playradio <radio_name>`: Play a predefined or custom radio station.\n" +
			"- `!stop`: Stop playing and disconnect the bot from the voice channel.\n" +
			"- `!listradios`: List all available radio stations.\n" +
			"- `!volume <0-100>`: Set the volume level.\n" +
			"- `!searchradio <keywords>`: Search for radio stations by keywords.\n" +
			"- `!playstation <number>`: Play a radio station from the search results.\n" +
			"- `!addradio <stream_url> <radio_name>`: Add a custom radio station.\n" +
			"- `!help`: Display this help message."

		s.ChannelMessageSend(m.ChannelID, helpMessage)
		return
	}

	if strings.HasPrefix(m.Content, "!playradio") {
		args := strings.Fields(m.Content)
		if len(args) < 2 {
			s.ChannelMessageSend(m.ChannelID, "Please specify a radio to play. For example: `!playradio gaucha`")
			return
		}

		radioName := strings.ToLower(args[1])

		streamURL, ok := streamURLs[radioName]
		if !ok {
			customRadiosMutex.RLock()
			streamURL, ok = customRadios[radioName]
			customRadiosMutex.RUnlock()
			if !ok {
				s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Unknown radio station: %s", radioName))
				return
			}
		}

		playRadioStream(s, m, streamURL, radioName)
	} else if m.Content == "!stop" {

		mutex.Lock()
		conn, ok := connections[m.GuildID]
		if !ok || !conn.streaming {
			mutex.Unlock()
			s.ChannelMessageSend(m.ChannelID, "Nothing is playing.")
			return
		}

		close(conn.stop)
		<-conn.done
		delete(connections, m.GuildID)
		mutex.Unlock()

		s.ChannelMessageSend(m.ChannelID, "Stopped playing.")
	} else if m.Content == "!listradios" {

		radios := make([]string, 0, len(streamURLs))
		for name := range streamURLs {
			radios = append(radios, name)
		}
		customRadiosMutex.RLock()
		for name := range customRadios {
			radios = append(radios, name)
		}
		customRadiosMutex.RUnlock()
		s.ChannelMessageSend(m.ChannelID, "Available radios: "+strings.Join(radios, ", "))
	} else if strings.HasPrefix(m.Content, "!volume") {

		args := strings.Fields(m.Content)
		if len(args) < 2 {
			s.ChannelMessageSend(m.ChannelID, "Please specify a volume level between 0 and 100.")
			return
		}

		volumeStr := args[1]
		volumeValue, err := strconv.Atoi(volumeStr)
		if err != nil || volumeValue < 0 || volumeValue > 100 {
			s.ChannelMessageSend(m.ChannelID, "Volume must be a number between 0 and 100.")
			return
		}

		mutex.Lock()
		conn, ok := connections[m.GuildID]
		mutex.Unlock()
		if !ok || !conn.streaming {
			s.ChannelMessageSend(m.ChannelID, "Nothing is playing.")
			return
		}

		conn.volumeMu.Lock()
		conn.volume = float64(volumeValue) / 100.0
		conn.volumeMu.Unlock()

		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Volume set to %d%%.", volumeValue))
	} else if strings.HasPrefix(m.Content, "!searchradio") {
		args := strings.Fields(m.Content)
		if len(args) < 2 {
			s.ChannelMessageSend(m.ChannelID, "Please provide keywords to search for radio stations.")
			return
		}

		query := strings.Join(args[1:], " ")
		stations, err := searchRadioStations(query)
		if err != nil {
			s.ChannelMessageSend(m.ChannelID, "Error searching for radio stations.")
			log.Println("Error searching for radio stations:", err)
			return
		}

		if len(stations) == 0 {
			s.ChannelMessageSend(m.ChannelID, "No radio stations found for your query.")
			return
		}

		response := "Found the following stations:\n"
		for i, station := range stations {
			response += fmt.Sprintf("%d. %s\n", i+1, station.Name)
			if i >= 9 {
				break
			}
		}
		response += "\nUse `!playstation <number>` to play a station."

		s.ChannelMessageSend(m.ChannelID, response)

		searchResultsMutex.Lock()
		searchResults[m.Author.ID] = stations
		searchResultsMutex.Unlock()
	} else if strings.HasPrefix(m.Content, "!playstation") {
		args := strings.Fields(m.Content)
		if len(args) < 2 {
			s.ChannelMessageSend(m.ChannelID, "Please specify the number of the station to play.")
			return
		}

		index, err := strconv.Atoi(args[1])
		if err != nil {
			s.ChannelMessageSend(m.ChannelID, "Invalid station number.")
			return
		}

		searchResultsMutex.Lock()
		stations, ok := searchResults[m.Author.ID]
		searchResultsMutex.Unlock()
		if !ok || len(stations) == 0 {
			s.ChannelMessageSend(m.ChannelID, "No search results found. Use `!searchradio` to search for stations.")
			return
		}

		if index < 1 || index > len(stations) {
			s.ChannelMessageSend(m.ChannelID, "Station number out of range.")
			return
		}

		station := stations[index-1]
		streamURL := station.URL

		playRadioStream(s, m, streamURL, station.Name)
	} else if strings.HasPrefix(m.Content, "!addradio") {

		args := strings.Fields(m.Content)
		if len(args) < 3 {
			s.ChannelMessageSend(m.ChannelID, "Usage: `!addradio <stream_url> <radio_name>`")
			return
		}

		streamURL := args[1]
		radioName := strings.ToLower(args[2])

		if !isValidURL(streamURL) {
			s.ChannelMessageSend(m.ChannelID, "Invalid stream URL.")
			return
		}

		customRadiosMutex.Lock()
		customRadios[radioName] = streamURL
		customRadiosMutex.Unlock()

		saveCustomRadios()

		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Custom radio `%s` added.", radioName))
	} else if strings.HasPrefix(m.Content, "!") {
		s.ChannelMessageSend(m.ChannelID, "Unknown command. Use `!help` to see the list of available commands.")
	}
}

func playRadioStream(s *discordgo.Session, m *discordgo.MessageCreate, streamURL, radioName string) {
	voiceChannelID := getUserVoiceChannelID(s, m.GuildID, m.Author.ID)
	if voiceChannelID == "" {
		s.ChannelMessageSend(m.ChannelID, "You must be in a voice channel to use this command.")
		return
	}

	mutex.Lock()

	if conn, ok := connections[m.GuildID]; ok {
		close(conn.stop)
		<-conn.done
		delete(connections, m.GuildID)
	}

	vc, err := s.ChannelVoiceJoin(m.GuildID, voiceChannelID, false, true)
	if err != nil {
		log.Println("Error joining voice channel:", err)
		s.ChannelMessageSend(m.ChannelID, "Error joining voice channel.")
		mutex.Unlock()
		return
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	conn := &Connection{
		vc:        vc,
		stop:      stop,
		done:      done,
		streaming: true,
		volume:    1.0,
	}
	connections[m.GuildID] = conn
	mutex.Unlock()

	go streamAudio(s, conn, streamURL)

	s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Now playing radio: %s", radioName))
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

func streamAudio(s *discordgo.Session, conn *Connection, streamURL string) {
	defer close(conn.done)
	defer conn.vc.Disconnect()

	vc := conn.vc

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
		log.Println("Error getting ffmpeg stdout:", err)
		return
	}

	err = ffmpeg.Start()
	if err != nil {
		log.Println("Error starting ffmpeg:", err)
		return
	}

	buffer := bufio.NewReaderSize(ffmpegOut, 16384)

	opusEncoder, err := gopus.NewEncoder(frameRate, channels, gopus.Audio)
	if err != nil {
		log.Fatal("NewEncoder Error: ", err)
	}

	vc.Speaking(true)
	defer vc.Speaking(false)

	errChan := make(chan error, 1)

	go func() {
		defer conn.vc.Disconnect()
		for {
			select {
			case <-conn.stop:
				log.Println("Stopping stream...")
				return
			default:
				conn.pauseMu.Lock()
				paused := conn.paused
				conn.pauseMu.Unlock()
				if paused {
					time.Sleep(1 * time.Second)
					continue
				}

				pcm := make([]int16, frameSize*channels)
				err = binary.Read(buffer, binary.LittleEndian, &pcm)
				if err != nil {
					if err == io.EOF {
						log.Println("Stream ended")
					} else {
						log.Println("Error reading stream data: ", err)
					}
					errChan <- err
					return
				}

				conn.volumeMu.RLock()
				volume := conn.volume
				conn.volumeMu.RUnlock()

				for i := range pcm {
					sample := float64(pcm[i]) * volume
					if sample > 32767 {
						sample = 32767
					} else if sample < -32768 {
						sample = -32768
					}
					pcm[i] = int16(sample)
				}

				opusData, err := opusEncoder.Encode(pcm, frameSize, maxBytes)
				if err != nil {
					log.Println("Error encoding PCM to Opus: ", err)
					errChan <- err
					return
				}

				if !vc.Ready || vc.OpusSend == nil {
					log.Println("Discord voice connection is not ready")
					errChan <- fmt.Errorf("Discord voice connection is not ready")
					return
				}
				vc.OpusSend <- opusData
			}
		}
	}()

	log.Println("Streaming started")

	select {
	case <-conn.stop:
		log.Println("Stream stopped by user")
	case err := <-errChan:
		log.Println("Stream stopped due to error:", err)
	}

	ffmpeg.Process.Kill()
	ffmpeg.Wait()
}

func searchRadioStations(query string) ([]RadioStation, error) {
	apiURL := "https://de1.api.radio-browser.info/json/stations/search"
	params := url.Values{}
	params.Set("name", query)
	params.Set("limit", "10")

	resp, err := http.Get(apiURL + "?" + params.Encode())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var stations []struct {
		Name        string `json:"name"`
		URLResolved string `json:"url_resolved"`
	}
	err = json.NewDecoder(resp.Body).Decode(&stations)
	if err != nil {
		return nil, err
	}

	result := make([]RadioStation, len(stations))
	for i, s := range stations {
		result[i] = RadioStation{
			Name: s.Name,
			URL:  s.URLResolved,
		}
	}

	return result, nil
}

func isValidURL(u string) bool {
	_, err := url.ParseRequestURI(u)
	return err == nil
}

func saveCustomRadios() {
	customRadiosMutex.RLock()
	defer customRadiosMutex.RUnlock()

	data, err := json.Marshal(customRadios)
	if err != nil {
		log.Println("Error marshalling custom radios:", err)
		return
	}

	err = os.WriteFile("radios.json", data, 0644)
	if err != nil {
		log.Println("Error writing custom radios to file:", err)
	}
}

func loadCustomRadios() {
	data, err := os.ReadFile("radios.json")
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		log.Println("Error reading custom radios file:", err)
		return
	}

	customRadiosMutex.Lock()
	defer customRadiosMutex.Unlock()

	err = json.Unmarshal(data, &customRadios)
	if err != nil {
		log.Println("Error unmarshalling custom radios:", err)
	}
}
