// Copyright 2016 VMware, Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cache

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"context"

	log "github.com/Sirupsen/logrus"

	"github.com/docker/distribution/digest"
	derr "github.com/docker/docker/errors"
	"github.com/docker/docker/pkg/truncindex"
	"github.com/docker/docker/reference"

	"github.com/vmware/vic/lib/apiservers/engine/backends/kv"
	"github.com/vmware/vic/lib/apiservers/portlayer/client"
	"github.com/vmware/vic/lib/metadata"
	"github.com/vmware/vic/pkg/trace"
)

// ICache is an in-memory cache of image metadata. It is refreshed at startup
// by a call to the portlayer. It is updated when new images are pulled or
// images are deleted.
type ICache struct {
	m           sync.RWMutex
	iDIndex     *truncindex.TruncIndex
	cacheByID   map[string]*metadata.ImageConfig
	cacheByName map[string]*metadata.ImageConfig

	client *client.PortLayer
}

const (
	imageCacheKey = "images"
)

var (
	imageCache *ICache
	ctx        = context.TODO()
)

func init() {
	imageCache = &ICache{
		iDIndex:     truncindex.NewTruncIndex([]string{}),
		cacheByID:   make(map[string]*metadata.ImageConfig),
		cacheByName: make(map[string]*metadata.ImageConfig),
	}
}

// ImageCache returns a reference to the image cache
func ImageCache() *ICache {
	return imageCache
}

// InitializeImageCache will create a new image cache or rehydrate an
// existing image cache from the portlayer k/v store
func InitializeImageCache(client *client.PortLayer) error {
	defer trace.End(trace.Begin(""))

	imageCache.client = client

	log.Debugf("Initializing image cache")

	val, err := kv.Get(client, imageCacheKey)
	if err != nil && err != kv.ErrKeyNotFound {
		return err
	}

	i := struct {
		IDIndex     *truncindex.TruncIndex
		CacheByID   map[string]*metadata.ImageConfig
		CacheByName map[string]*metadata.ImageConfig
	}{}

	if val != "" {

		if err = json.Unmarshal([]byte(val), &i); err != nil {
			return fmt.Errorf("Failed to unmarshal image cache: %s", err)
		}

		// populate the trie with IDs
		for k := range i.CacheByID {
			imageCache.iDIndex.Add(k)
		}

		imageCache.cacheByID = i.CacheByID
		imageCache.cacheByName = i.CacheByName
	}

	return nil
}

// GetImages returns a slice containing metadata for all cached images
func (ic *ICache) GetImages() []*metadata.ImageConfig {
	defer trace.End(trace.Begin(""))
	ic.m.RLock()
	defer ic.m.RUnlock()

	result := make([]*metadata.ImageConfig, 0, len(ic.cacheByID))
	for _, image := range ic.cacheByID {
		result = append(result, copyImageConfig(image))
	}
	return result
}

// IsImageID will check that a full or partial imageID
// exists in the cache
func (ic *ICache) IsImageID(id string) bool {
	ic.m.RLock()
	defer ic.m.RUnlock()
	if _, err := ic.iDIndex.Get(id); err == nil {
		return true
	}
	return false
}

// Get parses input to retrieve a cached image
func (ic *ICache) Get(idOrRef string) (*metadata.ImageConfig, error) {
	defer trace.End(trace.Begin(""))
	ic.m.RLock()
	defer ic.m.RUnlock()

	// cover the case of creating by a full reference
	if config, ok := ic.cacheByName[idOrRef]; ok {
		return copyImageConfig(config), nil
	}

	// get the full image ID if supplied a prefix
	if id, err := ic.iDIndex.Get(idOrRef); err == nil {
		idOrRef = id
	}

	imgDigest, named, err := reference.ParseIDOrReference(idOrRef)
	if err != nil {
		return nil, err
	}

	var config *metadata.ImageConfig
	if imgDigest != "" {
		config = ic.getImageByDigest(imgDigest)
	} else {
		config = ic.getImageByNamed(named)
	}

	if config == nil {
		// docker automatically prints out ":latest" tag if not specified in case if image is not found.
		postfixLatest := ""
		if !strings.Contains(idOrRef, ":") {
			postfixLatest += ":" + reference.DefaultTag
		}
		return nil, derr.NewRequestNotFoundError(fmt.Errorf(
			"No such image: %s%s", idOrRef, postfixLatest))
	}

	return copyImageConfig(config), nil
}

func (ic *ICache) getImageByDigest(digest digest.Digest) *metadata.ImageConfig {
	defer trace.End(trace.Begin(""))
	var config *metadata.ImageConfig
	config, ok := ic.cacheByID[string(digest)]
	if !ok {
		return nil
	}
	return copyImageConfig(config)
}

// Looks up image by reference.Named
func (ic *ICache) getImageByNamed(named reference.Named) *metadata.ImageConfig {
	defer trace.End(trace.Begin(""))
	// get the imageID from the repoCache
	id, _ := RepositoryCache().Get(named)
	return copyImageConfig(ic.cacheByID[prefixImageID(id)])
}

// Add the "sha256:" prefix to the image ID if missing.
// Don't assume the image id in image has "sha256:<id> as format.  We store it in
// this format to make it easier to lookup by digest
func prefixImageID(imageID string) string {
	if strings.HasPrefix(imageID, "sha256:") {
		return imageID
	}
	return "sha256:" + imageID
}

// Add adds an image to the image cache
func (ic *ICache) Add(imageConfig *metadata.ImageConfig) {
	defer trace.End(trace.Begin(""))

	ic.m.Lock()
	defer ic.m.Unlock()

	// Normalize the name stored in imageConfig using Docker's reference code
	ref, err := reference.WithName(imageConfig.Name)
	if err != nil {
		log.Errorf("Tried to create reference from %s: %s", imageConfig.Name, err.Error())
		return
	}

	imageID := prefixImageID(imageConfig.ImageID)
	ic.iDIndex.Add(imageConfig.ImageID)
	ic.cacheByID[imageID] = imageConfig

	for _, tag := range imageConfig.Tags {
		ref, err = reference.WithTag(ref, tag)
		if err != nil {
			log.Errorf("Tried to create tagged reference from %s and tag %s: %s", imageConfig.Name, tag, err.Error())
			return
		}
		ic.cacheByName[imageConfig.Reference] = imageConfig
	}
}

// RemoveImageByConfig removes image from the cache.
func (ic *ICache) RemoveImageByConfig(imageConfig *metadata.ImageConfig) {
	defer trace.End(trace.Begin(""))
	ic.m.Lock()
	defer ic.m.Unlock()

	// If we get here we definitely want to remove image config from any data structure
	// where it can be present. So that, if there is something is wrong
	// it could be tracked on debug level.
	if err := ic.iDIndex.Delete(imageConfig.ImageID); err != nil {
		log.Debugf("Not found in image cache index: %v", err)
	}

	prefixedID := prefixImageID(imageConfig.ImageID)
	if _, ok := ic.cacheByID[prefixedID]; ok {
		delete(ic.cacheByID, prefixedID)
	} else {
		log.Debugf("Not found in cache by id: %s", prefixedID)
	}

	if _, ok := ic.cacheByName[imageConfig.Reference]; ok {
		delete(ic.cacheByName, imageConfig.Reference)
	} else {
		log.Debugf("Not found in cache by name: %s", imageConfig.Reference)
	}
}

// Save will persist the image cache to the portlayer k/v store
func (ic *ICache) Save() error {
	defer trace.End(trace.Begin(""))
	ic.m.Lock()
	defer ic.m.Unlock()

	m := struct {
		IDIndex     *truncindex.TruncIndex
		CacheByID   map[string]*metadata.ImageConfig
		CacheByName map[string]*metadata.ImageConfig
	}{
		ic.iDIndex,
		ic.cacheByID,
		ic.cacheByName,
	}

	bytes, err := json.Marshal(m)
	if err != nil {
		log.Errorf("Unable to marshal image cache: %s", err.Error())
		return err
	}

	err = kv.Put(ic.client, imageCacheKey, string(bytes))
	if err != nil {
		log.Errorf("Unable to save image cache: %s", err.Error())
		return err
	}

	return nil
}

// copyImageConfig performs and returns deep copy of an ImageConfig struct
func copyImageConfig(image *metadata.ImageConfig) *metadata.ImageConfig {

	if image == nil {
		return nil
	}

	// copy everything
	newImage := *image

	// replace the pointer to metadata.ImageConfig.Config and copy the contents
	newConfig := *image.Config
	newImage.Config = &newConfig

	// get tags and digests from repo
	tags := RepositoryCache().Tags(newImage.ImageID)
	digests := RepositoryCache().Digests(newImage.ImageID)

	// if image has neither then set <none> vals
	if len(tags) == 0 && len(digests) == 0 {
		tags = append(tags, "<none>:<none>")
		digests = append(digests, "<none>@<none>")
	}

	newImage.Tags = tags
	newImage.Digests = digests

	return &newImage
}
