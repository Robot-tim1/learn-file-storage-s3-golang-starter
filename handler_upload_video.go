package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

type Streams struct {
	Streams []MetaData `json:"streams"`
}

type MetaData struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

func getVideoAspectRatio(filePath string) (string, error) {
	excCommand := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var buf bytes.Buffer
	excCommand.Stdout = &buf
	err := excCommand.Run()
	if err != nil {
		return "", fmt.Errorf("error running ffprobe on %s: %w", filePath, err)
	}

	var streams Streams
	if err = json.Unmarshal(buf.Bytes(), &streams); err != nil {
		return "", fmt.Errorf("error converting ffprobe output to json: %w", err)
	}

	result := streams.Streams[0].Width / streams.Streams[0].Height
	if result == 0 {
		return "9:16", nil
	}
	if result == 1 {
		return "16:9", nil
	}
	return "other", nil
}

func processVideoForFastStart(filePath string) (string, error) {
	outputFile := filePath + ".processing"

	excCommand := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputFile)
	err := excCommand.Run()
	if err != nil {
		return "", fmt.Errorf("error faststarting %s: %w", filePath, err)
	}

	return outputFile, nil
}

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	dbVideo, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't fetch video", err)
		return
	}

	if dbVideo.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Not authorized to update this video", nil)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	rawHeader := header.Header.Get("Content-Type")
	if rawHeader == "" {
		respondWithError(w, http.StatusBadRequest, "Missing Content-Type for video", nil)
		return
	}

	mediaType, _, err := mime.ParseMediaType(rawHeader)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't parse mime media type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Content-Type is not a mp4", nil)
		return
	}

	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	_, err = io.Copy(tempFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't copy data to file on file system", err)
		return
	}

	byteSlice := make([]byte, 32)
	_, err = rand.Read(byteSlice)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate internal byte slice", err)
		return
	}
	videoName := base64.RawURLEncoding.EncodeToString(byteSlice)

	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get aspect ratio of video", err)
		return
	}

	folder := aspectRatio
	switch aspectRatio {
	case "16:9":
		folder = "landscape"
	case "9:16":
		folder = "portrait"
	}

	videoType := strings.TrimPrefix(mediaType, "video/")
	Key := filepath.Join(folder + "/" + videoName + "." + videoType)

	tempFile.Seek(0, io.SeekStart)

	outputFilepath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process video for fast start", err)
		return
	}

	newFile, err := os.Open(outputFilepath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't open processed video filepath", err)
		return
	}
	defer os.Remove(newFile.Name())
	defer newFile.Close()

	bucketName := "tubely-543923"
	objectParams := s3.PutObjectInput{
		Bucket:      &bucketName,
		Key:         &Key,
		Body:        newFile,
		ContentType: &mediaType,
	}
	cfg.s3Client.PutObject(context.Background(), &objectParams)

	videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", bucketName, cfg.s3Region, Key)

	dbVideo.VideoURL = &videoURL
	err = cfg.db.UpdateVideo(dbVideo)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, dbVideo)
}
