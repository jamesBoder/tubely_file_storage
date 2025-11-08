package main

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
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

	// debug print after copy
	fmt.Println("Copied uploaded file to temp file")

	//get aspect ratio of the video file. Depending on the aspect ratio, add "landscape", "portrait", or "other" prefix to the key
	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to get video aspect ratio", err)
		return
	}

	// debug print aspect ratio
	fmt.Println("Video aspect ratio:", aspectRatio)

	// determine prefix based on aspect ratio
	aspectRatioPrefix := "other"
	switch aspectRatio {
	case "16:9", "4:3":
		aspectRatioPrefix = "landscape"
	case "9:16", "3:4":
		aspectRatioPrefix = "portrait"
	}
	fmt.Println("aspect ratio:", aspectRatio, "prefix:", aspectRatioPrefix)

	// debug print temp.seek
	fmt.Println("Seeked temp file to beginning")

	// reset the tempFile's file pointer to the beginning with .Seek(0,io.SeekStart)
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to seek temp file", err)
		return
	}

	// generate random 32 byte hex filename
	randomBytes := make([]byte, 32)
	_, err = crand.Read(randomBytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error generating random filename", err)
		return
	}
	// make a 64-char lowercase hex string using hex encoding
	randomHexFilename := hex.EncodeToString(randomBytes)

	// debug print before upload to S3
	fmt.Println("Uploading file to S3 with key:", randomHexFilename+".mp4")

	// upload the file to S3 with aspect ratio prefix in the path
	s3Key := fmt.Sprintf("videos/%s/%s.mp4", aspectRatioPrefix, randomHexFilename)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
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

// create getVideoAspectRatio. Takes a file path and returns the aspect ratio as a string
func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}

	var ff struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			Tags      struct {
				Rotate string `json:"rotate"`
			} `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out.Bytes(), &ff); err != nil {
		return "", err
	}

	// Go
	for _, s := range ff.Streams {
		if s.CodecType != "video" {
			continue
		}
		if s.Width <= 0 || s.Height <= 0 {
			continue
		}
		w, h := s.Width, s.Height
		if s.Tags.Rotate == "90" || s.Tags.Rotate == "270" {
			w, h = h, w
		}

		// debug print width and height
		fmt.Printf("Video width: %d, height: %d\n", w, h)

		ar := float64(w) / float64(h)

		if ar > 1.6 && ar < 1.85 {
			return "16:9", nil
		}
		if ar > 1.28 && ar < 1.36 {
			return "4:3", nil
		}
		if ar > 0.53 && ar < 0.62 {
			return "9:16", nil
		}
		if ar > 0.73 && ar < 0.82 {
			return "3:4", nil
		}
	}
	// no match found
	return "other", nil
}
