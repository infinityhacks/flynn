package main

import (
	"bytes"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/flynn/flynn/controller/client"
	ct "github.com/flynn/flynn/controller/types"
	"github.com/flynn/flynn/host/types"
	"github.com/flynn/flynn/pinkerton"
	"github.com/flynn/flynn/pkg/dialer"
	"github.com/flynn/flynn/pkg/imagebuilder"
)

func main() {
	log.SetFlags(0)

	if len(os.Args) != 2 {
		log.Fatalf("usage: %s URL", os.Args[0])
	}
	if err := run(os.Args[1]); err != nil {
		log.Fatalln("ERROR:", err)
	}
}

func run(url string) error {
	client, err := controller.NewClient("", os.Getenv("CONTROLLER_KEY"))
	if err != nil {
		return err
	}

	if err := os.MkdirAll("/var/lib/docker", 0755); err != nil {
		return err
	}
	context, err := pinkerton.BuildContext("flynn", "/var/lib/docker")
	if err != nil {
		return err
	}

	layerDir := "/var/lib/flynn"
	if err := os.MkdirAll(layerDir, 0755); err != nil {
		return err
	}

	builder := &imagebuilder.Builder{
		LayerDir: layerDir,
		Context:  context,
	}

	// pull the docker image
	ref, err := pinkerton.NewRef(url)
	if err != nil {
		return err
	}
	if _, err := context.PullDocker(url, os.Stdout); err != nil {
		return err
	}

	// create squashfs for each layer
	image, err := builder.Build(ref.DockerRef(), false)
	if err != nil {
		return err
	}

	// upload layers + manifest to blobstore
	for _, layer := range image.Rootfs[0].Layers {
		id := layer.Hashes["sha512"]
		f, err := os.Open(filepath.Join(layerDir, id+".squashfs"))
		if err != nil {
			return err
		}
		defer f.Close()
		layer.URL = fmt.Sprintf("http://blobstore.discoverd/docker/layers/%s.squashfs", id)
		if err := upload(f, layer.URL); err != nil {
			return err
		}
	}
	imageData, err := json.Marshal(image)
	if err != nil {
		return err
	}
	imageHash := sha512.Sum512(imageData)
	imageURL := fmt.Sprintf("http://blobstore.discoverd/docker/images/%s.json", hex.EncodeToString(imageHash[:]))

	if err := upload(bytes.NewReader(imageData), imageURL); err != nil {
		return err
	}

	// create the artifact
	artifact := &ct.Artifact{
		Type: host.ArtifactTypeFlynn,
		URI:  imageURL,
		Meta: map[string]string{
			"blobstore":                 "true",
			"docker-receive.repository": ref.Name(),
			"docker-receive.digest":     ref.ID(),
		},
		Manifest: image,
	}
	return client.CreateArtifact(artifact)
}

func upload(data io.Reader, url string) error {
	req, err := http.NewRequest("PUT", url, data)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{Dial: dialer.Retry.Dial}}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected HTTP status: %s", res.Status)
	}
	return nil
}
