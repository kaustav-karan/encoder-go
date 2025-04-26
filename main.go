package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var (
	minioEndpoint  string
	minioAccessKey string
	minioSecretKey string
	minioBucket    string
	useSSL         bool
)

func init() {
	err := godotenv.Load()
	if err != nil {
		log.Println("Warning: .env file not found, using default values")
	}

	minioEndpoint = os.Getenv("MINIO_ENDPOINT") + ":" + os.Getenv("MINIO_PORT")
	if minioEndpoint == ":" {
		minioEndpoint = "localhost:9000"
	}

	minioAccessKey = os.Getenv("MINIO_ACCESS_KEY")
	if minioAccessKey == "" {
		minioAccessKey = "minioadmin"
	}

	minioSecretKey = os.Getenv("MINIO_SECRET_KEY")
	if minioSecretKey == "" {
		minioSecretKey = "minioadmin"
	}

	minioBucket = os.Getenv("MINIO_BUCKET")
	if minioBucket == "" {
		minioBucket = "hls-audio"
	}

	useSSL = os.Getenv("USE_SSL") == "true"
}

func main() {
	http.HandleFunc("/convert", handleConvert)
	fmt.Println("Server started at 0.0.0.0:8080")
	http.ListenAndServe("0.0.0.0:8080", nil)
}

func uploadToMinio(folder string, objectPrefix string) error {
    ctx := context.Background()

    client, err := minio.New(minioEndpoint, &minio.Options{
        Creds:  credentials.NewStaticV4(minioAccessKey, minioSecretKey, ""),
        Secure: useSSL,
    })
    if err != nil {
        return err
    }

    exists, err := client.BucketExists(ctx, minioBucket)
    if err != nil {
        return err
    }
    if !exists {
        err = client.MakeBucket(ctx, minioBucket, minio.MakeBucketOptions{})
        if err != nil {
            return err
        }
    }

    // Create the folder structure by ensuring the prefix ends with /
    if !strings.HasSuffix(objectPrefix, "/") {
        objectPrefix = objectPrefix + "/"
    }

    entries, err := os.ReadDir(folder)
    if err != nil {
        return err
    }

    for _, entry := range entries {
        if entry.IsDir() {
            continue
        }
        
        // Simplified file naming
        var objectName string
        switch {
        case strings.Contains(entry.Name(), "input"):
            objectName = objectPrefix + "input.mp3"
        case strings.Contains(entry.Name(), "output"):
            objectName = objectPrefix + "output.m3u8"
        case strings.Contains(entry.Name(), "segment"):
            objectName = objectPrefix + entry.Name() // keeps segment_001.ts etc.
        default:
            objectName = objectPrefix + entry.Name()
        }

        filePath := filepath.Join(folder, entry.Name())
        
        // Set appropriate content types
        opts := minio.PutObjectOptions{}
        if strings.HasSuffix(objectName, ".m3u8") {
            opts.ContentType = "application/vnd.apple.mpegurl"
        } else if strings.HasSuffix(objectName, ".ts") {
            opts.ContentType = "video/MP2T"
        }

        _, err := client.FPutObject(ctx, minioBucket, objectName, filePath, opts)
        if err != nil {
            log.Println("Upload failed for:", filePath, err)
            return err
        }
        log.Println("Uploaded:", objectName)
    }

    return nil
}

func handleConvert(w http.ResponseWriter, r *http.Request) {
    presignedURL := r.URL.Query().Get("url")
    if presignedURL == "" {
        http.Error(w, "Missing 'url' query parameter", http.StatusBadRequest)
        return
    }

    // Create working directory with simple name
    workingDir := filepath.Join(os.TempDir(), "hls-conversion")
    err := os.MkdirAll(workingDir, 0755)
    if err != nil {
        http.Error(w, "Failed to create temp directory", http.StatusInternalServerError)
        return
    }
    defer os.RemoveAll(workingDir)

    // Download input file
    inputPath := filepath.Join(workingDir, "input.mp3")
    err = downloadFile(inputPath, presignedURL)
    if err != nil {
        http.Error(w, "Failed to download file: "+err.Error(), http.StatusInternalServerError)
        return
    }

    // Convert to HLS
    outputPath := filepath.Join(workingDir, "output.m3u8")
    segmentPattern := filepath.Join(workingDir, "segment_%03d.ts")
    cmd := exec.Command("ffmpeg",
        "-i", inputPath,
        "-c:a", "aac", "-b:a", "192k",
        "-f", "hls",
        "-hls_time", "5",
        "-hls_list_size", "0",
        "-hls_flags", "independent_segments+append_list",
        "-hls_segment_filename", segmentPattern,
        outputPath,
    )
    cmd.Stdout = os.Stdout
    cmd.Stderr = os.Stderr
    err = cmd.Run()
    if err != nil {
        http.Error(w, "FFmpeg conversion failed: "+err.Error(), http.StatusInternalServerError)
        return
    }

    // Upload to MinIO with simple folder structure
    folderName := "converted-audio/" // Fixed folder name
    err = uploadToMinio(workingDir, folderName)
    if err != nil {
        http.Error(w, "Upload to MinIO failed: "+err.Error(), http.StatusInternalServerError)
        return
    }

    protocol := "http"
    if useSSL {
        protocol = "https"
    }

    // Generate the public URL
    publicM3U8URL := fmt.Sprintf("%s://%s/%s/%soutput.m3u8", protocol, minioEndpoint, minioBucket, folderName)
    log.Println("Stream available at:", publicM3U8URL)
    w.WriteHeader(http.StatusOK)
    w.Write([]byte(fmt.Sprintf("✅ Conversion successful!\nStream: %s", publicM3U8URL)))
}

func downloadFile(filepath string, url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}
