package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
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

	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)

	// TODO: implement the upload here
	const maxMemory = 10 << 20
	err = r.ParseMultipartForm(maxMemory)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form", err)
		return
	}

	file, header, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "unable to parse form file", err)
		return
	}

	defer file.Close()
	contentType := header.Header.Get("Content-Type")

	media, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid media type", err)
		return
	}
	if media != "image/jpeg" && media != "image/png" {
		respondWithError(w, http.StatusBadRequest, "Invalid media type", fmt.Errorf("Invalid media type"))
		return
	}

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Video not found", err)
		return
	}

	if video.UserID != userID {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	fields := strings.Split(media, "/")
	if len(fields) != 2 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ext := fields[1]

	randomByte := make([]byte, 32)
	rand.Read(randomByte)
	fileName := base64.RawURLEncoding.EncodeToString(randomByte)
	fileNameWithExt := fmt.Sprintf("%s.%s", fileName, ext)

	filePath := filepath.Join(cfg.assetsRoot, fileNameWithExt)
	newFile, err := os.Create(filePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to create file", err)
		return
	}
	defer newFile.Close()

	_, err = io.Copy(newFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to read image", err)
		return
	}

	tbURL := fmt.Sprintf(
		"http://localhost:%s/assets/%s",
		cfg.port,
		fileNameWithExt,
	)

	video.ThumbnailURL = &tbURL
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Server error", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}
