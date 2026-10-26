package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	// Step 1: Limit the HTTP request body to 1 GiB
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)

	// Step 2: Parse video ID
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid video ID", err)
		return
	}

	// Step 3: Authenticate user
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

	// Step 4: Retrieve video metadata
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Video not found", err)
		return
	}

	// Verify video ownership
	if video.UserID != userID {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Step 5: Retrieve uploaded file
	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to retrieve video file", err)
		return
	}
	defer file.Close()

	// Step 6: Validate MIME type
	contentType := header.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid media type", err)
		return
	}

	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Only MP4 videos are allowed", nil)
		return
	}

	// Step 7: Create a temporary file
	tempFile, err := os.CreateTemp("", "tubely-upload-*.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to create temporary file", err)
		return
	}

	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	// Copy uploaded video into temporary file
	_, err = io.Copy(tempFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to copy video", err)
		return
	}
	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to determine aspect ratio", err)
		return
	}

	// Process the video for fast start
	processedPath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to process video", err)
		return
	}
	defer os.Remove(processedPath)

	// Open the processed video
	processedFile, err := os.Open(processedPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to open processed video", err)
		return
	}
	defer processedFile.Close()

	// NEW: Get processed file size
	fileInfo, err := processedFile.Stat()
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to get file size", err)
		return
	}

	// Step 9: Generate random object key
	randomBytes := make([]byte, 32)
	_, err = rand.Read(randomBytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to generate object key", err)
		return
	}

	fileName := hex.EncodeToString(randomBytes) + ".mp4"

	var prefix string

	switch aspectRatio {
	case "16:9":
		prefix = "landscape"
	case "9:16":
		prefix = "portrait"
	default:
		prefix = "other"
	}

	objectKey := fmt.Sprintf("%s/%s", prefix, fileName)

	// Upload video to S3
	_, err = cfg.s3Client.PutObject(
		r.Context(),
		&s3.PutObjectInput{
			Bucket:        aws.String(cfg.s3Bucket),
			Key:           aws.String(objectKey),
			Body:          processedFile,
			ContentType:   aws.String(mediaType),
			ContentLength: aws.Int64(fileInfo.Size()),
		},
	)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to upload video to S3", err)
		return
	}

	videoURL := fmt.Sprintf("%s/%s", cfg.s3CfDistribution, objectKey)
	video.VideoURL = &videoURL

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to update video metadata", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var output bytes.Buffer
	cmd.Stdout = &output
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("Error executing the command: %s", err)
	}

	// Define struct for JSON output
	var result struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
	}
	// Decode JSON
	err = json.Unmarshal(output.Bytes(), &result)
	if err != nil {
		return "", err
	}

	// Find the video stream
	for _, stream := range result.Streams {
		if stream.CodecType != "video" {
			continue
		}

		if stream.Width == 0 || stream.Height == 0 {
			return "", fmt.Errorf("invalid video dimensions")
		}

		ratio := float64(stream.Width) / float64(stream.Height)

		if math.Abs(ratio-16.0/9.0) < 0.01 {
			return "16:9", nil
		}

		if math.Abs(ratio-9.0/16.0) < 0.01 {
			return "9:16", nil
		}

		return "other", nil
	}

	return "", fmt.Errorf("no video stream found")
}

func processVideoForFastStart(filePath string) (string, error) {
	outputPath := fmt.Sprintf("%s.processing", filePath)
	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputPath)
	err := cmd.Run()
	if err != nil {
		os.Remove(outputPath)
		return "", err
	}
	return outputPath, nil
}
