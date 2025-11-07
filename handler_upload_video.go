package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

// store files in S3. images stay on local file system for now
func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	// set upload size to 1GB using http.MaxBytesReader
	const maxUploadSize = 1 << 30 // 1 GB
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	// extract the videoID from the URL path and parse it as a UUID
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	// Authenticate the user to get a userID
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

	// fetch the video row and verify the user owns it
	fmt.Println("uploading video for video", videoID, "by user", userID)

	// verify video ownership. compare to userID

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Video not found", err)
		return
	}

	if video.UserID != userID {
		respondWithError(w, http.StatusForbidden, "You do not own this video", nil)
		return
	}

	// parse the multipart form with max memory of 32MB
	const maxMemory = 32 << 20 // 32 MB
	err = r.ParseMultipartForm(maxMemory)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to parse multipart form", err)
		return
	}
	// TODO: extract the video file from the multipart form

	file, fileHeader, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Failed to retrieve video file", err)
		return
	}

	// debug print after formfile
	fmt.Println("Retrieved file:", fileHeader.Filename, "size:", fileHeader.Size)

	defer file.Close()

	// validate the media type to ensure it's a MP4 video. using mime.ParseMediaType. not from header but from file
	mediaType, _, err := mime.ParseMediaType(fileHeader.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid file type", nil)
		return
	}

	// upload file to a temp file on disk first. use os.CreateTemp
	tempFile, err := os.CreateTemp("", "tubely-upload-*.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to create temp file", err)
		return
	}

	// debug print temp file name
	fmt.Println("Created temp file:", tempFile.Name())

	// defer remove the temp with os.Remove
	defer func() {
		tempFile.Close()
		os.Remove(tempFile.Name())
	}()

	// copy the uploaded file to the temp file
	_, err = io.Copy(tempFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to save uploaded file", err)
		return
	}

	// reset the tempFile's file pointer to the beginning with .Seek(0,io.SeekStart)
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to seek temp file", err)
		return
	}

	// debug print temp.seek
	fmt.Println("Seeked temp file to beginning")

	// generate random 32 byte hex filename
	randomBytes := make([]byte, 32)
	_, err = rand.Read(randomBytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error generating random filename", err)
		return
	}
	// make a 64-char lowercase hex string using hex encoding
	randomFilename := hex.EncodeToString(randomBytes)

	// debug print before upload to S3
	fmt.Println("Uploading file to S3 with key:", randomFilename+".mp4")

	// upload the file to S3
	s3Key := fmt.Sprintf("videos/%s.mp4", randomFilename)

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	// debug print
	// go
	fmt.Println("S3 bucket:", cfg.s3Bucket, "region:", cfg.s3Region, "key:", s3Key)

	_, err = cfg.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(s3Key),
		Body:        tempFile,
		ContentType: aws.String(mediaType),
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to upload file to S3", err)
		return
	}

	// debug print after upload
	fmt.Println("Successfully uploaded file to S3 with key:", s3Key)

	// update the video's VideoURL field to the S3 URL and return a success JSON response
	videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, s3Key)
	u := videoURL
	video.VideoURL = &u
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to update video URL in database", err)
		return
	}

	response := map[string]string{
		"message":   "Video uploaded successfully",
		"video_url": videoURL,
	}
	respondWithJSON(w, http.StatusOK, response)

}
