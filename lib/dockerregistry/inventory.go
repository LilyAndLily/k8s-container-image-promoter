/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package inventory

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	yaml "gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/google"
	cr "github.com/google/go-containerregistry/pkg/v1/types"
	cipJson "sigs.k8s.io/k8s-container-image-promoter/lib/json"
	"sigs.k8s.io/k8s-container-image-promoter/lib/stream"
	"sigs.k8s.io/k8s-container-image-promoter/pkg/gcloud"
)

func getSrcRegistry(rcs []RegistryContext) (*RegistryContext, error) {
	for _, registry := range rcs {
		registry := registry
		if registry.Src {
			return &registry, nil
		}
	}
	return nil, fmt.Errorf("could not find source registry")
}

// MakeSyncContext creates a SyncContext.
func MakeSyncContext(
	mfests []Manifest,
	verbosity, threads int,
	dryRun, useSvcAcc bool) (SyncContext, error) {

	sc := SyncContext{
		Verbosity:         verbosity,
		Threads:           threads,
		DryRun:            dryRun,
		UseServiceAccount: useSvcAcc,
		Inv:               make(MasterInventory),
		InvIgnore:         []ImageName{},
		Tokens:            make(map[RootRepo]gcloud.Token),
		RegistryContexts:  make([]RegistryContext, 0),
		DigestMediaType:   make(DigestMediaType),
		ParentDigest:      make(ParentDigest)}

	registriesSeen := make(map[RegistryContext]interface{})
	for _, mfest := range mfests {
		for _, r := range mfest.Registries {
			registriesSeen[r] = nil
		}
	}

	// Populate SyncContext with registries found across all manifests.
	for r := range registriesSeen {
		sc.RegistryContexts = append(sc.RegistryContexts, r)
	}

	// Sort the list for determinism. We first sort it alphabetically, then sort
	// it by length (reverse order, so that the longest registry names come
	// first). This is so that we try to match the leading prefix against the
	// longest registry names first. We sort alphabetically first because we
	// want the final order to be deterministic.
	sort.Slice(sc.RegistryContexts, func(i, j int) bool {
		return sc.RegistryContexts[i].Name < sc.RegistryContexts[j].Name
	})
	sort.Slice(sc.RegistryContexts, func(i, j int) bool {
		// nolint[lll]
		return len(sc.RegistryContexts[i].Name) > len(sc.RegistryContexts[j].Name)
	})

	// Populate access tokens for all registries listed in the manifest.
	if useSvcAcc {
		err := sc.PopulateTokens()
		if err != nil {
			return SyncContext{}, err
		}
	}

	return sc, nil
}

// ParseManifestFromFile parses a Manifest from a filepath.
func ParseManifestFromFile(filePath string) (Manifest, error) {

	var mfest Manifest
	var empty Manifest

	b, err := ioutil.ReadFile(filePath)
	if err != nil {
		return empty, err
	}

	mfest, err = ParseManifestYAML(b)
	if err != nil {
		return empty, err
	}

	mfest.filepath = filePath

	err = mfest.finalize()
	if err != nil {
		return empty, err
	}

	return mfest, nil
}

// ParseThinManifestFromFile parses a ThinManifest from a filepath and generates
// a Manifest.
func ParseThinManifestFromFile(filePath string) (Manifest, error) {

	var thinManifest ThinManifest
	var mfest Manifest
	var empty Manifest

	b, err := ioutil.ReadFile(filePath)
	if err != nil {
		return empty, err
	}

	thinManifest, err = ParseThinManifestYAML(b)
	if err != nil {
		return empty, err
	}

	imagesPath := thinManifest.ImagesPath
	// Handle a relative path for "ImagesPath", if necessary.
	if strings.HasPrefix(thinManifest.ImagesPath, ".") {
		imagesPath = filepath.Join(
			filepath.Dir(filePath),
			thinManifest.ImagesPath)
	}
	images, err := ParseImagesFromFile(imagesPath)
	if err != nil {
		return empty, err
	}

	mfest.filepath = filePath
	mfest.Images = images
	mfest.Registries = thinManifest.Registries

	err = mfest.finalize()
	if err != nil {
		return empty, err
	}

	return mfest, nil
}

// ParseImagesFromFile parses an Images type from a file.
func ParseImagesFromFile(filePath string) (Images, error) {
	var images Images
	var empty Images

	b, err := ioutil.ReadFile(filePath)
	if err != nil {
		return empty, err
	}

	images, err = ParseImagesYAML(b)
	if err != nil {
		return empty, err
	}

	return images, nil
}

func (m *Manifest) finalize() error {
	// Perform semantic checks (beyond just YAML validation).
	srcRegistry, err := getSrcRegistry(m.Registries)
	if err != nil {
		return err
	}
	m.srcRegistry = srcRegistry

	return nil
}

// ParseManifestsFromDir parses all Manifest files within a directory. We
// effectively have to create a map of manifests, keyed by the source registry
// (there can only be 1 source registry).
func ParseManifestsFromDir(
	dir string,
	parseManifestFunc func(string) (Manifest, error)) ([]Manifest, error) {
	mfests := make([]Manifest, 0)

	var parseAsManifest filepath.WalkFunc = func(path string,
		info os.FileInfo,
		err error) error {

		if err != nil {
			// Prevent panic in case of incoming errors accessing this path.
			klog.Errorf("failure accessing a path %q: %v\n", path, err)
		}

		// Skip directories (because they are not YAML files).
		if info.IsDir() {
			return nil
		}

		// First try to parse the path as a manifest file, which must be named
		// "promoter-manifest.yaml". This restriction is in place to limit the
		// scope of what is read in as a promoter manifest.
		if filepath.Base(path) != "promoter-manifest.yaml" {
			return nil
		}

		mfest, errParse := parseManifestFunc(path)
		if errParse != nil {
			klog.Errorf("could not parse manifest file '%s'\n", path)
			return errParse
		}

		// Save successful parse result.
		mfests = append(mfests, mfest)

		return nil
	}

	if err := filepath.Walk(dir, parseAsManifest); err != nil {
		return mfests, err
	}

	if len(mfests) == 0 {
		return nil, fmt.Errorf("no manifests found in dir: %s", dir)
	}

	return mfests, nil
}

// ToPromotionEdges converts a list of manifests to a set of edges we want to
// try promoting.
func ToPromotionEdges(
	mfests []Manifest) (map[PromotionEdge]interface{}, error) {
	edges := make(map[PromotionEdge]interface{})
	// nolint[lll]
	for _, mfest := range mfests {
		for _, image := range mfest.Images {
			for digest, tagArray := range image.Dmap {
				for _, destRC := range mfest.Registries {
					if destRC == *mfest.srcRegistry {
						continue
					}

					if len(tagArray) > 0 {
						for _, tag := range tagArray {
							edge := mkPromotionEdge(
								*mfest.srcRegistry,
								destRC,
								image.ImageName,
								digest,
								tag)
							edges[edge] = nil
						}
					} else {
						// If this digest does not have any associated tags,
						// still create a promotion edge for it (tagless
						// promotion).
						edge := mkPromotionEdge(
							*mfest.srcRegistry,
							destRC,
							image.ImageName,
							digest,
							"") // No associated tag; still promote!
						edges[edge] = nil
					}
				}
			}
		}
	}

	return checkOverlappingEdges(edges)
}

func mkPromotionEdge(
	srcRC, dstRC RegistryContext,
	srcImageName ImageName,
	digest Digest,
	tag Tag) PromotionEdge {

	edge := PromotionEdge{
		SrcRegistry: srcRC,
		SrcImageTag: ImageTag{
			ImageName: srcImageName,
			Tag:       tag},
		Digest:      digest,
		DstRegistry: dstRC,
	}

	// The name in the destination is the same as the name in the source.
	edge.DstImageTag = ImageTag{
		ImageName: srcImageName,
		Tag:       tag}
	return edge
}

// This filters out those edges from ToPromotionEdges (found in []Manifest), to
// only those PromotionEdges that makes sense to keep around. For example, we
// want to remove all edges that have already been promoted.
//
// nolint[funlen]
// nolint[gocyclo]
func (sc *SyncContext) getPromotionCandidates(
	edges map[PromotionEdge]interface{}) map[PromotionEdge]interface{} {

	// Create lookup-optimized structure for images to ignore.
	ignoreMap := make(map[ImageName]interface{})
	for _, ignoreMe := range sc.InvIgnore {
		ignoreMap[ignoreMe] = nil
	}

	toPromote := make(map[PromotionEdge]interface{})
	// nolint[lll]
	for edge := range edges {
		// If the edge should be ignored because of a bad read in sc.Inv, drop
		// it (complain with klog though).
		if img, ok := ignoreMap[edge.SrcImageTag.ImageName]; ok {
			klog.Warningf("edge %s: ignoring because src image could not be read: %s\n", edge, img)
			continue
		}

		sp, dp := edge.VertexProps(sc.Inv)

		// If dst vertex exists, NOP.
		if dp.PqinDigestMatch {
			klog.Infof("edge %s: skipping because it was already promoted (case 1)\n", edge)
			continue
		}

		// If this edge is for a tagless promotion, skip if the digest exists in
		// the destination.
		if edge.DstImageTag.Tag == "" && dp.DigestExists {
			// Still, log a warning if the source is missing the image.
			if !sp.DigestExists {
				klog.Errorf("edge %s: skipping %s/%s@%s because it was already promoted, but it is still _LOST_ (can't find it in src registry! please backfill it!)\n", edge, edge.SrcRegistry.Name, edge.SrcImageTag.ImageName, edge.Digest)
			}
			continue
		}

		// If src vertex missing, LOST && NOP. We just need the digest to exist
		// in src (we don't care if it points to the wrong tag).
		if !sp.DigestExists {
			klog.Errorf("edge %s: skipping %s/%s@%s because it is _LOST_ (can't find it in src registry!)\n", edge, edge.SrcRegistry.Name, edge.SrcImageTag.ImageName, edge.Digest)
			continue
		}

		if dp.PqinExists {
			if dp.DigestExists {
				// NOP (already promoted).
				klog.Infof("edge %s: skipping because it was already promoted (case 2)\n", edge)
				continue
			} else {
				// Pqin points to the wrong digest.
				klog.Warningf("edge %s: tag %s points to the wrong digest; moving\n, dp.BadDigest")
			}
		} else {
			if dp.DigestExists {
				// Digest exists in dst, but the pqin we desire does not
				// exist. Just add the pqin to this existing digest.
				klog.Infof("edge %s: digest %s already exists, but does not have the pqin we want (%s)\n", edge, dp.OtherTags)
			} else {
				// Neither the digest nor the pqin exists in dst.
				klog.Infof("edge %s: regular promotion (neither digest nor pqin exists in dst)\n", edge)
			}
		}

		toPromote[edge] = nil
	}

	return toPromote
}

// checkOverlappingEdges checks to ensure that all the edges taken together as a
// whole are consistent. It checks that there are no duplicate promotions
// desired to the same destination vertex (same destination PQIN). If the
// digests are the same for those edges, ignore because by definition the
// digests are cryptographically guaranteed to be the same thing (it doesn't
// matter if 2 different parties want the same exact image to become promoted to
// the same destination --- in many ways this is actually a good thing because
// it's a form of redundancy). However, return an error if the digests are
// different, because most likely this is at best just human error and at worst
// a malicious attack (someone trying to push an image to an endpoint they
// shouldn't own).
//
// nolint[gocyclo]
func checkOverlappingEdges(
	edges map[PromotionEdge]interface{}) (map[PromotionEdge]interface{}, error) {

	// Build up a "promotionIntent". This will be checked below.
	promotionIntent := make(map[string]map[Digest][]PromotionEdge)
	checked := make(map[PromotionEdge]interface{})
	for edge := range edges {
		// Skip overlap checks for edges that are tagless, because by definition
		// they cannot overlap with another edge.
		if edge.DstImageTag.Tag == "" {
			checked[edge] = nil
			continue
		}

		dstPQIN := ToPQIN(edge.DstRegistry.Name,
			edge.DstImageTag.ImageName,
			edge.DstImageTag.Tag)

		digestToEdges, ok := promotionIntent[dstPQIN]
		if ok {
			// Store the edge.
			digestToEdges[edge.Digest] = append(digestToEdges[edge.Digest], edge)
			promotionIntent[dstPQIN] = digestToEdges
		} else {
			// Make this edge lay claim to this destination vertex.
			edgeList := make([]PromotionEdge, 0)
			edgeList = append(edgeList, edge)
			digestToEdges := make(map[Digest][]PromotionEdge)
			digestToEdges[edge.Digest] = edgeList
			promotionIntent[dstPQIN] = digestToEdges
		}
	}

	// Review the promotionIntent to ensure that there are no issues.
	overlapError := false
	emptyEdgeListError := false
	for pqin, digestToEdges := range promotionIntent {
		if len(digestToEdges) < 2 {
			for _, edgeList := range digestToEdges {
				switch len(edgeList) {
				case 0:
					klog.Errorf("no edges for %v", pqin)
					emptyEdgeListError = true
				case 1:
					checked[edgeList[0]] = nil
				default:
					klog.Infof("redundant promotion: multiple edges want to promote the same digest to the same destination endpoint %v:", pqin)
					for _, edge := range edgeList {
						klog.Infof("%v", edge)
					}
					klog.Infof("using the first one: %v", edgeList[0])
					checked[edgeList[0]] = nil
				}
			}
		} else {
			klog.Errorf("multiple edges want to promote *different* images (digests) to the same destination endpoint %v:", pqin)
			for digest, edgeList := range digestToEdges {
				klog.Errorf("  for digest %v:\n", digest)
				for _, edge := range edgeList {
					klog.Errorf("%v\n", edge)
				}
			}
			overlapError = true
		}
	}

	if overlapError {
		return nil, fmt.Errorf("overlapping edges detected")
	}

	if emptyEdgeListError {
		return nil, fmt.Errorf("empty edgeList(s) detected")
	}

	return checked, nil
}

// VertexProps determines the properties of each vertex (src and dst) in the
// edge, depending on the state of the world in the MasterInventory.
func (edge PromotionEdge) VertexProps(
	mi MasterInventory) (VertexProperty, VertexProperty) {

	d := edge.VertexPropsFor(edge.DstRegistry, edge.DstImageTag, mi)
	s := edge.VertexPropsFor(edge.SrcRegistry, edge.SrcImageTag, mi)

	return s, d
}

// VertexPropsFor examines one of the two vertices (src or dst) of a
// PromotionEdge.
func (edge PromotionEdge) VertexPropsFor(
	rc RegistryContext,
	imageTag ImageTag,
	mi MasterInventory) VertexProperty {

	p := VertexProperty{}

	rii, ok := mi[rc.Name]
	if !ok {
		return p
	}
	digestTags, ok := rii[imageTag.ImageName]
	if !ok {
		return p
	}

	if tagSlice, ok := digestTags[edge.Digest]; ok {
		p.DigestExists = true
		// Record the tags that are associated with this digest; it may turn out
		// that within this tagslice, we indeed have the correct digest, in
		// which we set it back to an empty slice.
		p.OtherTags = tagSlice
	}

	for digest, tagSlice := range digestTags {
		for _, tag := range tagSlice {
			if tag == imageTag.Tag {
				p.PqinExists = true
				if digest == edge.Digest {
					p.PqinDigestMatch = true
					// Both the digest and tag match what we wanted in the
					// imageTag, so there are no extraneous tags to bother with.
					p.OtherTags = TagSlice{}
				} else {
					p.BadDigest = digest
				}
			}
		}
	}

	return p
}

// ParseManifestYAML parses a Manifest from a byteslice. This function is
// separate from ParseManifestFromFile() so that it can be tested independently.
func ParseManifestYAML(b []byte) (Manifest, error) {
	var m Manifest
	if err := yaml.UnmarshalStrict(b, &m); err != nil {
		return m, err
	}

	return m, m.Validate()
}

// ParseThinManifestYAML parses a ThinManifest from a byteslice.
func ParseThinManifestYAML(b []byte) (ThinManifest, error) {
	var m ThinManifest
	if err := yaml.UnmarshalStrict(b, &m); err != nil {
		return m, err
	}

	return m, nil
}

// ParseImagesYAML parses Images from a byteslice.
func ParseImagesYAML(b []byte) (Images, error) {
	var images Images
	if err := yaml.UnmarshalStrict(b, &images); err != nil {
		return images, err
	}

	return images, nil
}

// Validate checks for semantic errors in the yaml fields (the structure of the
// yaml is checked during unmarshaling).
func (m Manifest) Validate() error {
	if err := validateRequiredComponents(m); err != nil {
		return err
	}
	return validateImages(m.Images)
}

func validateImages(images []Image) error {
	for _, image := range images {
		for digest, tagSlice := range image.Dmap {
			if err := validateDigest(digest); err != nil {
				return err
			}

			for _, tag := range tagSlice {
				if err := validateTag(tag); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateDigest(digest Digest) error {
	validDigest := regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	if !validDigest.Match([]byte(digest)) {
		return fmt.Errorf("invalid digest: %v", digest)
	}
	return nil
}

func validateTag(tag Tag) error {
	var validTag = regexp.MustCompile(`^[\w][\w.-]{0,127}$`)
	if !validTag.Match([]byte(tag)) {
		return fmt.Errorf("invalid tag: %v", tag)
	}
	return nil
}

func validateRegistryImagePath(rip RegistryImagePath) error {
	validRegistryImagePath := regexp.MustCompile(
		// \w is [0-9a-zA-Z_]
		`^[\w-]+(\.[\w-]+)+(/[\w-]+)+$`)
	if !validRegistryImagePath.Match([]byte(rip)) {
		return fmt.Errorf("invalid registry image path: %v", rip)
	}
	return nil
}

func (m Manifest) srcRegistryCount() int {
	var count int
	for _, registry := range m.Registries {
		if registry.Src {
			count++
		}
	}
	return count
}

func (m Manifest) srcRegistryName() RegistryName {
	for _, registry := range m.Registries {
		if registry.Src {
			return registry.Name
		}
	}
	return RegistryName("")
}

// nolint[gocyclo]
func validateRequiredComponents(m Manifest) error {
	errs := make([]string, 0)
	srcRegistryName := RegistryName("")
	if len(m.Registries) > 0 {
		if m.srcRegistryCount() > 1 {
			errs = append(errs, fmt.Sprintf("cannot have more than 1 source registry"))
		}
		srcRegistryName = m.srcRegistryName()
		if len(srcRegistryName) == 0 {
			errs = append(errs, fmt.Sprintf("source registry must be set"))
		}
	}

	knownRegistries := make([]RegistryName, 0)
	if len(m.Registries) == 0 {
		errs = append(errs, fmt.Sprintf("'registries' field cannot be empty"))
	}
	for _, registry := range m.Registries {
		if len(registry.Name) == 0 {
			errs = append(
				errs,
				fmt.Sprintf("registries: 'name' field cannot be empty"))
		}
		knownRegistries = append(knownRegistries, registry.Name)
	}
	for _, image := range m.Images {
		if len(image.ImageName) == 0 {
			errs = append(
				errs,
				fmt.Sprintf("images: 'name' field cannot be empty"))
		}
		if len(image.Dmap) == 0 {
			errs = append(
				errs,
				fmt.Sprintf("images: 'dmap' field cannot be empty"))
		}
	}

	if len(errs) == 0 {
		return nil
	}

	return fmt.Errorf(strings.Join(errs, "\n"))
}

// PrettyValue creates a prettified string representation of MasterInventory.
//
// nolint[gocyclo]
func (mi *MasterInventory) PrettyValue() string {
	var b strings.Builder
	regNames := []RegistryName{}
	for regName := range *mi {
		regNames = append(regNames, regName)
	}
	sort.Slice(regNames, func(i, j int) bool {
		return regNames[i] < regNames[j]
	})

	for _, regName := range regNames {
		v, ok := (*mi)[regName]
		if !ok {
			klog.Error("corrupt MasterInventory")
			return ""
		}
		fmt.Fprintln(&b, regName)
		imageNamesSorted := make([]string, 0)
		for imageName := range v {
			imageNamesSorted = append(imageNamesSorted, string(imageName))
		}
		sort.Strings(imageNamesSorted)
		for _, imageName := range imageNamesSorted {
			fmt.Fprintf(&b, "  %v\n", imageName)
			digestTags, ok := v[ImageName(imageName)]
			if !ok {
				continue
			}
			digestSorted := make([]string, 0)
			for digest := range digestTags {
				digestSorted = append(digestSorted, string(digest))
			}
			sort.Strings(digestSorted)
			for _, digest := range digestSorted {
				fmt.Fprintf(&b, "    %v\n", digest)
				tags, ok := digestTags[Digest(digest)]
				if !ok {
					continue
				}
				for _, tag := range tags {
					fmt.Fprintf(&b, "      %v\n", tag)
				}
			}
		}
	}
	return b.String()
}

// PrettyValue converts a Registry to a prettified string representation.
func (r *Registry) PrettyValue() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%v (%v)\n", r.RegistryNameLong, r.RegistryName)
	fmt.Fprintln(&b, r.RegInvImageDigest.PrettyValue())
	return b.String()
}

// PrettyValue converts a RegInvImageDigest to a prettified string
// representation.
func (riid *RegInvImageDigest) PrettyValue() string {
	var b strings.Builder
	ids := make([]ImageDigest, 0)
	for id := range *riid {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		iID := string(ids[i].ImageName) + string(ids[i].Digest)
		jID := string(ids[j].ImageName) + string(ids[i].Digest)
		return iID < jID
	})
	for _, id := range ids {
		fmt.Fprintf(&b, "  %v\n", id.ImageName)
		fmt.Fprintf(&b, "    %v\n", id.Digest)
		tagArray, ok := (*riid)[id]
		if !ok {
			continue
		}
		tagArraySorted := make([]string, 0)
		for _, tag := range tagArray {
			tagArraySorted = append(tagArraySorted, string(tag))
		}
		sort.Strings(tagArraySorted)
		for _, tag := range tagArraySorted {
			fmt.Fprintf(&b, "      %v\n", tag)
		}
	}
	return b.String()
}

func getRegistryTagsWrapper(req stream.ExternalRequest) (*google.Tags, error) {

	var googleTags *google.Tags

	var getRegistryTagsCondition wait.ConditionFunc = func() (bool, error) {
		var err error

		googleTags, err = getRegistryTagsFrom(req)

		// We never return an error (err) in the second part of our return
		// argument, because we don't want to prematurely stop the
		// ExponentialBackoff() loop; we want it to continue looping until
		// either we get a well-formed tags value, or until it hits
		// ErrWaitTimeout. This is how ExponentialBackoff() uses the
		// ConditionFunc type.
		if err == nil && googleTags != nil && len(googleTags.Name) > 0 {
			return true, nil
		}
		return false, nil
	}

	err := wait.ExponentialBackoff(
		stream.BackoffDefault,
		getRegistryTagsCondition)

	if err != nil {
		klog.Error(err)
		return nil, err
	}

	return googleTags, nil
}

func getRegistryTagsFrom(req stream.ExternalRequest) (*google.Tags, error) {
	reader, _, err := req.StreamProducer.Produce()
	if err != nil {
		klog.Warning("error reading from stream:", err)
		// Skip google.Tags JSON parsing if there were errors reading from the
		// HTTP stream.
		return nil, err
	}

	// nolint[errcheck]
	defer req.StreamProducer.Close()

	tags, err := extractRegistryTags(reader)
	if err != nil {
		klog.Warning("error parsing *google.Tags from io.Reader handle:", err)
		return nil, err
	}

	return tags, nil
}

func getGCRManifestListWrapper(
	req stream.ExternalRequest) (*GCRManifestList, error) {

	var gcrManifestList *GCRManifestList

	var getGCRManifestListCondition wait.ConditionFunc = func() (bool, error) {
		var err error

		gcrManifestList, err = getGCRManifestListFrom(req)

		// We never return an error (err) in the second part of our return
		// argument, because we don't want to prematurely stop the
		// ExponentialBackoff() loop; we want it to continue looping until
		// either we get a well-formed value, or until it hits
		// ErrWaitTimeout. This is how ExponentialBackoff() uses the
		// ConditionFunc type.
		if err == nil &&
			gcrManifestList != nil &&
			len(gcrManifestList.Manifests) > 0 {

			return true, nil
		}
		// nolint[lll]
		klog.Errorf("invalid gcrManifestList state: %s for request %s", gcrManifestList, req)
		return false, nil
	}

	err := wait.ExponentialBackoff(
		stream.BackoffDefault,
		getGCRManifestListCondition)

	if err != nil {
		klog.Error(err)
		return nil, err
	}

	return gcrManifestList, nil
}

func getGCRManifestListFrom(
	req stream.ExternalRequest) (*GCRManifestList, error) {

	reader, _, err := req.StreamProducer.Produce()
	if err != nil {
		klog.Warning("error reading from stream:", err)
		return nil, err
	}

	// nolint[errcheck]
	defer req.StreamProducer.Close()

	gcrManifestList, err := extractGCRManifestList(reader)
	if err != nil {
		// nolint[lll]
		klog.Warningf("for request %s: error parsing GCRManifestList from io.Reader handle: %s", req.RequestParams, err)
		return nil, err
	}

	return gcrManifestList, nil
}

func getJSONSFromProcess(req stream.ExternalRequest) (cipJson.Objects, Errors) {
	var jsons cipJson.Objects
	errors := make(Errors, 0)
	stdoutReader, stderrReader, err := req.StreamProducer.Produce()
	if err != nil {
		errors = append(errors, Error{
			Context: "running process",
			Error:   err})
	}
	jsons, err = cipJson.Consume(stdoutReader)
	if err != nil {
		errors = append(errors, Error{
			Context: "parsing JSON",
			Error:   err})
	}
	be, err := ioutil.ReadAll(stderrReader)
	if err != nil {
		errors = append(errors, Error{
			Context: "reading process stderr",
			Error:   err})
	}
	if len(be) > 0 {
		errors = append(errors, Error{
			Context: "process had stderr",
			Error:   fmt.Errorf("%v", string(be))})
	}
	err = req.StreamProducer.Close()
	if err != nil {
		errors = append(errors, Error{
			Context: "closing process",
			Error:   err})
	}
	return jsons, errors
}

// IgnoreFromPromotion works by building up a new Inv type of those images that
// should NOT be bothered to be Promoted; these get ignored in the Promote()
// step later down the pipeline.
func (sc *SyncContext) IgnoreFromPromotion(regName RegistryName) {
	// regName will look like gcr.io/foo/bar/baz. We then look for the key
	// "foo/bar/baz".
	_, imgName, err := ParseContainerParts(string(regName))
	if err != nil {
		klog.Errorf("unable to ignore from promotion: %s\n", err)
		return
	}

	klog.Infof("ignoring from promotion: %s\n", imgName)
	sc.InvIgnore = append(sc.InvIgnore, ImageName(imgName))
}

// ParseContainerParts splits up a registry name into its component pieces.
// Unfortunately it has some specialized logic around particular inputs; this
// could be removed in a future promoter manifest version which could force the
// user to provide these delineations for us.
//
// nolint[gocyclo]
func ParseContainerParts(s string) (string, string, error) {
	parts := strings.Split(s, "/")
	if len(parts) <= 1 {
		goto error
	}
	// String may not have a double slash, or a trailing slash (which would
	// result in an empty substring).
	for _, part := range parts {
		if len(part) == 0 {
			goto error
		}
	}
	switch parts[0] {
	case "gcr.io":
		fallthrough
	case "asia.gcr.io":
		fallthrough
	case "eu.gcr.io":
		fallthrough
	case "us.gcr.io":
		if len(parts) == 2 {
			goto error
		}
		return strings.Join(parts[0:2], "/"), strings.Join(parts[2:], "/"), nil
	case "k8s.gcr.io":
		fallthrough
	case "staging-k8s.gcr.io":
		fallthrough
	default:
		return parts[0], strings.Join(parts[1:], "/"), nil
	}
error:
	return "", "", fmt.Errorf("invalid string '%s'", s)
}

// PopulateTokens populates the SyncContext's Tokens map with actual usable
// access tokens.
func (sc *SyncContext) PopulateTokens() error {
	for _, rc := range sc.RegistryContexts {
		token, err := gcloud.GetServiceAccountToken(
			rc.ServiceAccount,
			sc.UseServiceAccount)
		if err != nil {
			return err
		}
		tokenKey, _, _ := GetTokenKeyDomainRepoPath(rc.Name)
		sc.Tokens[RootRepo(tokenKey)] = token
	}

	return nil
}

// GetTokenKeyDomainRepoPath splits a string by '/'. It's OK to do this because
// the RegistryName is already parsed against a Regex. (Maybe we should store
// the repo path separately when we do the initial parse...)
func GetTokenKeyDomainRepoPath(
	registryName RegistryName) (string, string, string) {

	s := string(registryName)
	i := strings.IndexByte(s, '/')
	key := ""
	if strings.Count(s, "/") < 2 {
		key = s
	} else {
		key = strings.Join(strings.Split(s, "/")[0:2], "/")
	}
	// key, domain, repository path
	return key, s[:i], s[i+1:]
}

// ReadRegistries reads all images in all registries in the SyncContext Each
// registry is composed of a image repositories, which can be recursive.
//
// To summarize: a docker *registry* is a set of *repositories*. It just so
// happens that to end-users, repositores resemble a tree structure because they
// are delineated by familiar filesystem-like "directory" paths.
//
// We use the term "registry" to mean the "root repository" in this program, but
// to be technically correct, for gcr.io/google-containers/foo/bar/baz:
//
//  - gcr.io is the registry
//  - gcr.io/google-containers is the toplevel repository (or "root" repo)
//  - gcr.io/google-containers/foo is a child repository
//  - gcr.io/google-containers/foo/bar is a child repository
//  - gcr.io/google-containers/foo/bar/baz is a child repository
//
// It may or may not be the case that the child repository is empty. E.g., if
// only one image gcr.io/google-containers/foo/bar/baz:1.0 exists in the entire
// registry, the foo/ and bar/ subdirs are empty repositories.
//
// The root repo, or "registry" in the loose sense, is what we care about. This
// is because in GCR, each root repo is given its own service account and
// credentials that extend to all child repos. And also in GCR, the name of the
// root repo is the same as the name of the GCP project that hosts it.
//
// NOTE: Repository names may overlap with image names. E.g., it may be in the
// example above that there are images named gcr.io/google-containers/foo:2.0
// and gcr.io/google-containers/foo/baz:2.0.
//
// nolint[gocyclo]
func (sc *SyncContext) ReadRegistries(
	toRead []RegistryContext,
	recurse bool,
	mkProducer func(*SyncContext, RegistryContext) stream.Producer) {

	// Collect all images in sc.Inv (the src and dest registry names found in
	// the manifest).
	var populateRequests PopulateRequests = func(
		sc *SyncContext,
		reqs chan<- stream.ExternalRequest,
		wg *sync.WaitGroup) {

		// For each registry, start the very first root "repo" read call.
		for _, rc := range toRead {
			// Create the request.
			var req stream.ExternalRequest
			req.RequestParams = rc
			req.StreamProducer = mkProducer(sc, rc)
			// Load request into the channel.
			wg.Add(1)
			reqs <- req
		}
	}
	var processRequest ProcessRequest = func(
		sc *SyncContext,
		reqs chan stream.ExternalRequest,
		requestResults chan<- RequestResult,
		wg *sync.WaitGroup,
		mutex *sync.Mutex) {

		for req := range reqs {
			reqRes := RequestResult{Context: req}

			// Now run the request (make network HTTP call with
			// ExponentialBackoff()).
			tagsStruct, err := getRegistryTagsWrapper(req)
			if err != nil {
				// Skip this request if it has unrecoverable errors (even after
				// ExponentialBackoff).
				reqRes.Errors = Errors{
					Error{
						Context: "getRegistryTagsWrapper",
						Error:   err}}
				requestResults <- reqRes
				wg.Add(-1)

				// Invalidate promotion conservatively for the subset of images
				// that touch this network request. If we have trouble reading
				// "foo" from a destination registry, do not bother trying to
				// promote it for all registries
				mutex.Lock()
				sc.IgnoreFromPromotion(req.RequestParams.(RegistryContext).Name)
				mutex.Unlock()

				continue
			}
			// Process the current repo.
			rName := req.RequestParams.(RegistryContext).Name
			digestTags := make(DigestTags)

			for digest, mfestInfo := range tagsStruct.Manifests {
				tagSlice := TagSlice{}
				for _, tag := range mfestInfo.Tags {
					tagSlice = append(tagSlice, Tag(tag))
				}
				digestTags[Digest(digest)] = tagSlice

				// Store MediaType.
				mutex.Lock()
				mediaType, err := supportedMediaType(mfestInfo.MediaType)
				if err != nil {
					fmt.Printf("digest %s: %s\n", digest, err)
				}
				sc.DigestMediaType[Digest(digest)] = mediaType
				mutex.Unlock()
			}

			// Only write an entry into our inventory if the entry has some
			// non-nil value for digestTags. This is because we only want to
			// populate the inventory with image names that have digests in
			// them, and exclude any image paths that are actually just folder
			// names without any images in them.
			if len(digestTags) > 0 {
				rootReg, imageName, err := SplitByKnownRegistries(rName, sc.RegistryContexts)
				if err != nil {
					klog.Exitln(err)
				}

				currentRepo := make(RegInvImage)
				currentRepo[imageName] = digestTags

				mutex.Lock()
				existingRegEntry := sc.Inv[rootReg]
				if len(existingRegEntry) == 0 {
					sc.Inv[rootReg] = currentRepo
				} else {
					sc.Inv[rootReg][imageName] = digestTags
				}
				mutex.Unlock()
			}

			reqRes.Errors = Errors{}
			requestResults <- reqRes

			// Process child repos.
			if recurse {
				for _, childRepoName := range tagsStruct.Children {
					parentRC, _ := req.RequestParams.(RegistryContext)

					childRc := RegistryContext{
						Name: RegistryName(
							string(parentRC.Name) + "/" + childRepoName),
						// Inherit the service account used at the parent
						// (cascades down from toplevel to all subrepos). In the
						// future if the token exchange fails, we can refresh
						// the token here instead of using the one we inherit
						// below.
						ServiceAccount: parentRC.ServiceAccount,
						// Inherit the token as well.
						Token: parentRC.Token,
						// Don't need src, because we are just reading data
						// (don't care if it's the source reg or not).
					}

					var childReq stream.ExternalRequest
					childReq.RequestParams = childRc
					childReq.StreamProducer = mkProducer(sc, childRc)

					// Every time we "descend" into child nodes, increment the
					// semaphore.
					wg.Add(1)
					reqs <- childReq
				}
			}
			// When we're done processing this node (req), decrement the
			// semaphore.
			wg.Add(-1)
		}
	}
	sc.ExecRequests(populateRequests, processRequest)
}

// ReadGCRManifestLists reads all manifest lists and populates the ParentDigest
// field of the SyncContext. ParentDigest is a map of values of the form
// map[ChildDigest]ParentDigest; and so, if a digest has an entry in this map,
// it is referenced by a parent DockerManifestList.
//
// TODO: Combine this function with ReadRegistries().
//
// nolint[gocyclo]
func (sc *SyncContext) ReadGCRManifestLists(
	mkProducer func(*SyncContext, GCRManifestListContext) stream.Producer) {

	// Collect all images in sc.Inv (the src and dest registry names found in
	// the manifest).
	var populateRequests PopulateRequests = func(
		sc *SyncContext,
		reqs chan<- stream.ExternalRequest,
		wg *sync.WaitGroup) {

		// Find all images that are of cr.MediaType == DockerManifestList; these
		// images will be queried.
		for registryName, rii := range sc.Inv {
			var rc RegistryContext
			for _, registryContext := range sc.RegistryContexts {
				if registryContext.Name == registryName {
					rc = registryContext
				}
			}
			for imageName, digestTags := range rii {
				for digest, tagSlice := range digestTags {
					if sc.DigestMediaType[digest] == cr.DockerManifestList {
						// Create the request.
						var req stream.ExternalRequest
						var tag Tag
						if len(tagSlice) > 0 {
							// It could be that this ManifestList has been
							// tagged multiple times. Just grab the first tag.
							tag = tagSlice[0]
						}
						gmlc := GCRManifestListContext{
							RegistryContext: rc,
							ImageName:       imageName,
							Tag:             tag,
							Digest:          digest}
						req.RequestParams = gmlc
						req.StreamProducer = mkProducer(sc, gmlc)
						wg.Add(1)
						reqs <- req
					}
				}
			}
		}
	}

	var processRequest ProcessRequest = func(
		sc *SyncContext,
		reqs chan stream.ExternalRequest,
		requestResults chan<- RequestResult,
		wg *sync.WaitGroup,
		mutex *sync.Mutex) {

		for req := range reqs {
			reqRes := RequestResult{Context: req}

			// Now run the request (make network HTTP call with
			// ExponentialBackoff()).
			gcrManifestList, err := getGCRManifestListWrapper(req)
			if err != nil {
				// Skip this request if it has unrecoverable errors (even after
				// ExponentialBackoff).
				reqRes.Errors = Errors{
					Error{
						Context: "getGCRManifestListWrapper",
						Error:   err}}
				requestResults <- reqRes
				wg.Add(-1)

				continue
			}

			gmlc := req.RequestParams.(GCRManifestListContext)

			for _, gManifest := range gcrManifestList.Manifests {
				mutex.Lock()
				sc.ParentDigest[gManifest.Digest] = gmlc.Digest
				mutex.Unlock()
			}

			reqRes.Errors = Errors{}
			requestResults <- reqRes

			wg.Add(-1)
		}
	}
	sc.ExecRequests(populateRequests, processRequest)
}

// FilterByTag removes all images in RegInvImage that do not match the
// filterTag.
func FilterByTag(rii RegInvImage, filterTag string) RegInvImage {
	filtered := make(RegInvImage)
	for imageName, digestTags := range rii {
		for digest, tags := range digestTags {
			for _, tag := range tags {
				if string(tag) == filterTag {
					if filtered[imageName] == nil {
						filtered[imageName] = make(DigestTags)
					}
					filtered[imageName][digest] = append(
						filtered[imageName][digest],
						tag)
				}
			}
		}
	}
	return filtered
}

// RemoveChildDigestEntries removes all tagless images in RegInvImage that are
// referenced by ManifestLists in the Registries.
func (sc *SyncContext) RemoveChildDigestEntries(rii RegInvImage) RegInvImage {
	filtered := make(RegInvImage)
	for imageName, digestTags := range rii {
		for digest, tagSlice := range digestTags {
			_, hasParent := sc.ParentDigest[digest]
			// If this image digest is only referenced as part of a parent
			// ManfestList (i.e. not directly tagged), we filter it out.
			if hasParent && len(tagSlice) == 0 {
				continue
			}

			if filtered[imageName] == nil {
				filtered[imageName] = make(DigestTags)
			}
			filtered[imageName][digest] = tagSlice
		}
	}
	return filtered
}

// SplitByKnownRegistries splits a registry name into a RegistryName and
// ImageName.
func SplitByKnownRegistries(
	r RegistryName,
	rcs []RegistryContext) (RegistryName, ImageName, error) {

	for _, rc := range rcs {
		if strings.HasPrefix(string(r), string(rc.Name)) {
			trimmed := strings.TrimPrefix(string(r), string(rc.Name))

			// Remove leading "/" character, if any.
			if trimmed[0] == '/' {
				return rc.Name, ImageName(trimmed[1:]), nil
			}
			return rc.Name, ImageName(trimmed), nil
		}
	}

	return "", "", fmt.Errorf("unknown registry %q", r)
}

// MkReadRepositoryCmdReal creates a stream.Producer which makes a real call
// over the network.
func MkReadRepositoryCmdReal(
	sc *SyncContext,
	rc RegistryContext) stream.Producer {

	var sh stream.HTTP

	tokenKey, domain, repoPath := GetTokenKeyDomainRepoPath(rc.Name)

	httpReq, err := http.NewRequest(
		"GET",
		fmt.Sprintf("https://%s/v2/%s/tags/list", domain, repoPath),
		nil)

	if err != nil {
		klog.Fatalf(
			"could not create HTTP request for '%s/%s'",
			domain,
			repoPath)
	}

	if sc.UseServiceAccount {
		token, ok := sc.Tokens[RootRepo(tokenKey)]
		if !ok {
			klog.Exitf("access token for key '%s' not found\n", tokenKey)
		}

		rc.Token = token
		var bearer = "Bearer " + string(rc.Token)
		httpReq.Header.Add("Authorization", bearer)
	}

	sh.Req = httpReq
	return &sh
}

// MkReadManifestListCmdReal creates a stream.Producer which makes a real call
// over the network to read ManifestList information.
//
// TODO: Consider replacing stream.Producer return type with a simple ([]byte,
// error) tuple instead.
func MkReadManifestListCmdReal(
	sc *SyncContext,
	gmlc GCRManifestListContext) stream.Producer {

	var sh stream.HTTP

	tokenKey, domain, repoPath := GetTokenKeyDomainRepoPath(
		gmlc.RegistryContext.Name)

	endpoint := fmt.Sprintf(
		"https://%s/v2/%s/%s/manifests/%s",
		domain,
		repoPath,
		gmlc.ImageName,
		// Always refer by a digest, because it may be the case that this
		// manifest list is not actually tagged!
		gmlc.Digest)

	httpReq, err := http.NewRequest("GET", endpoint, nil)

	// Without this, GCR responds as we had used the "Accept:
	// application/vnd.docker.distribution.manifest.v1+prettyjws" header.
	httpReq.Header.Add("Accept", "*/*")

	if err != nil {
		klog.Fatalf(
			"could not create HTTP request for manifest list '%s/%s/%s:%s'",
			domain,
			repoPath,
			gmlc.ImageName,
			gmlc.Digest)
	}

	if sc.UseServiceAccount {
		token, ok := sc.Tokens[RootRepo(tokenKey)]
		if !ok {
			klog.Exitf("access token for key '%s' not found\n", tokenKey)
		}

		var bearer = "Bearer " + string(token)
		httpReq.Header.Add("Authorization", bearer)
	}

	sh.Req = httpReq
	return &sh
}

// ExecRequests uses the Worker Pool pattern, where MaxConcurrentRequests
// determines the number of workers to spawn.
func (sc *SyncContext) ExecRequests(
	populateRequests PopulateRequests,
	processRequest ProcessRequest) {
	// Run requests.
	MaxConcurrentRequests := 10
	if sc.Threads > 0 {
		MaxConcurrentRequests = sc.Threads
	}
	mutex := &sync.Mutex{}
	reqs := make(chan stream.ExternalRequest, MaxConcurrentRequests)
	requestResults := make(chan RequestResult)
	// We have to use a WaitGroup, because even though we know beforehand the
	// number of workers, we don't know the number of jobs.
	wg := new(sync.WaitGroup)
	// Log any errors encountered.
	go func() {
		for reqRes := range requestResults {
			if len(reqRes.Errors) > 0 {
				klog.Errorf(
					"Request %v: error(s) encountered: %v\n",
					reqRes.Context,
					reqRes.Errors)
			} else {
				klog.Infof("Request %v: OK\n", reqRes.Context.RequestParams)
			}
		}
	}()
	for w := 0; w < MaxConcurrentRequests; w++ {
		go processRequest(sc, reqs, requestResults, wg, mutex)
	}
	// This can't be a goroutine, because the semaphore could be 0 by the time
	// wg.Wait() is called. So we need to block against the initial "seeding" of
	// workloads into the reqs channel.
	populateRequests(sc, reqs, wg)

	// Wait for all workers to finish draining the jobs.
	wg.Wait()
	close(reqs)

	// Close requestResults channel because no more new jobs are being created
	// (it's OK to close a channel even if it has nonzero length). On the other
	// hand, we cannot close the channel before we Wait() for the workers to
	// finish, because we would end up closing it too soon, and a worker would
	// end up trying to send a result to the already-closed channel.
	//
	// NOTE: This requestResults channel is only useful if we want a central
	// place to process how each request happened (good for maybe debugging slow
	// reqs? benchmarking?). If we just want to print something, for example,
	// whenever there's an error, we could do away with this channel and just
	// spit out to STDOUT wheneven we encounter an error, from whichever
	// goroutine (no need to put the error into a channel for consumption from a
	// single point).
	close(requestResults)
}

func extractRegistryTags(reader io.Reader) (*google.Tags, error) {

	tags := google.Tags{}
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()

	for {
		err := decoder.Decode(&tags)
		if err != nil {
			if err == io.EOF {
				break
			}
			klog.Error("DECODING ERROR: ", err)
			return nil, err
		}
	}
	return &tags, nil
}

func extractGCRManifestList(reader io.Reader) (*GCRManifestList, error) {

	gcrManifestList := GCRManifestList{}
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()

	for {
		err := decoder.Decode(&gcrManifestList)
		if err != nil {
			if err == io.EOF {
				break
			}
			klog.Error("DECODING ERROR: ", err)
			return nil, err
		}
	}
	return &gcrManifestList, nil
}

// Overwrite insert's b's values into a.
func (a DigestTags) Overwrite(b DigestTags) {
	for k, v := range b {
		a[k] = v
	}
}

// ToFQIN combines a RegistryName, ImageName, and Digest to form a
// fully-qualified image name (FQIN).
func ToFQIN(registryName RegistryName,
	imageName ImageName,
	digest Digest) string {

	return string(registryName) + "/" + string(imageName) + "@" + string(digest)
}

// ToPQIN converts a RegistryName, ImageName, and Tag to form a
// partially-qualified image name (PQIN). It's less exact than a FQIN because
// the digest information is not used.
func ToPQIN(registryName RegistryName, imageName ImageName, tag Tag) string {
	return string(registryName) + "/" + string(imageName) + ":" + string(tag)
}

// ToSorted converts a RegInvImage type to a sorted structure.
func (rii *RegInvImage) ToSorted() []ImageWithDigestSlice {
	images := make([]ImageWithDigestSlice, 0)

	for name, dmap := range *rii {
		var digests []digest
		for k, v := range dmap {
			var tags []string
			for _, tag := range v {
				tags = append(tags, string(tag))
			}

			sort.Strings(tags)

			digests = append(digests, digest{
				hash: string(k),
				tags: tags,
			})
		}
		sort.Slice(digests, func(i, j int) bool {
			return digests[i].hash < digests[j].hash
		})

		images = append(images, ImageWithDigestSlice{
			name:    string(name),
			digests: digests,
		})
	}

	sort.Slice(images, func(i, j int) bool {
		return images[i].name < images[j].name
	})

	return images
}

// ToYAML displays a RegInvImage as YAML, but with the map items sorted
// alphabetically.
func (rii *RegInvImage) ToYAML() string {
	images := rii.ToSorted()

	var b strings.Builder
	for _, image := range images {
		fmt.Fprintf(&b, "- name: %s\n", image.name)
		fmt.Fprintf(&b, "  dmap:\n")
		for _, digestEntry := range image.digests {
			fmt.Fprintf(&b, "    %s:", digestEntry.hash)
			if len(digestEntry.tags) > 0 {
				fmt.Fprintf(&b, "\n")
				for _, tag := range digestEntry.tags {
					fmt.Fprintf(&b, "    - %s\n", tag)
				}
			} else {
				fmt.Fprintf(&b, " []\n")
			}
		}
	}

	return b.String()
}

// ToCSV is like ToYAML, but instead of printing things in an indented
// format, it prints one image on each line as a CSV. If there is a tag pointing
// to the image, then it is printed next to the image on the same line.
//
// E.g.
//
// nolint[lll]
// a@sha256:0000000000000000000000000000000000000000000000000000000000000000,a:1.0
// a@sha256:0000000000000000000000000000000000000000000000000000000000000000,a:latest
// b@sha256:1111111111111111111111111111111111111111111111111111111111111111,-
func (rii *RegInvImage) ToCSV() string {
	images := rii.ToSorted()

	var b strings.Builder
	for _, image := range images {
		for _, digestEntry := range image.digests {
			if len(digestEntry.tags) > 0 {
				for _, tag := range digestEntry.tags {
					fmt.Fprintf(&b, "%s@%s,%s:%s\n",
						image.name,
						digestEntry.hash,
						image.name,
						tag)
				}
			} else {
				fmt.Fprintf(&b, "%s@%s,-\n", image.name, digestEntry.hash)
			}
		}
	}

	return b.String()
}

// ToLQIN converts a RegistryName and ImangeName to form a loosely-qualified
// image name (LQIN). Notice that it is missing tag information --- hence
// "loosely-qualified".
func ToLQIN(registryName RegistryName, imageName ImageName) string {
	return string(registryName) + "/" + string(imageName)
}

// SplitRegistryImagePath takes an arbitrary image path, and splits it into its
// component parts, according to the knownRegistries field. E.g., consider
// "gcr.io/foo/a/b/c" as the registryImagePath. If "gcr.io/foo" is in
// knownRegistries, then we split it into "gcr.io/foo" and "a/b/c". But if we
// were given "gcr.io/foo/a", we would split it into "gcr.io/foo/a" and "b/c".
func SplitRegistryImagePath(
	registryImagePath RegistryImagePath,
	knownRegistries []RegistryName) (RegistryName, ImageName, error) {

	for _, rName := range knownRegistries {
		if strings.HasPrefix(string(registryImagePath), string(rName)) {
			return rName, ImageName(registryImagePath[len(rName)+1:]), nil
		}
	}

	return RegistryName(""),
		ImageName(""),
		// nolint[lll]
		fmt.Errorf("could not determine registry name for '%v'", registryImagePath)
}

// nolint[lll]
func mkPopulateRequestsForPromotionEdges(
	toPromote map[PromotionEdge]interface{},
	mkProducer PromotionContext) PopulateRequests {
	return func(sc *SyncContext, reqs chan<- stream.ExternalRequest, wg *sync.WaitGroup) {
		if len(toPromote) == 0 {
			klog.Info("Nothing to promote.")
			return
		}

		if sc.DryRun {
			klog.Info("---------- BEGIN PROMOTION (DRY RUN) ----------")
		} else {
			klog.Info("---------- BEGIN PROMOTION ----------")
		}

		for promoteMe := range toPromote {
			var req stream.ExternalRequest
			tp := Add
			oldDigest := Digest("")

			_, dp := promoteMe.VertexProps(sc.Inv)

			if dp.PqinExists {
				if !dp.DigestExists {
					// Pqin points to the wrong digest.
					klog.Warningf("edge %s: tag %s points to the wrong digest; moving\n, dp.BadDigest")
					tp = Move
					oldDigest = dp.BadDigest
				}
			}

			// Do not invoke mkProducer for Add or Move operations.
			if tp == Delete {
				req.StreamProducer = mkProducer(
					// TODO: Clean up types to avoid having to split up promoteMe
					// prematurely like this.
					promoteMe.SrcRegistry.Name,
					promoteMe.SrcImageTag.ImageName,
					promoteMe.DstRegistry,
					promoteMe.DstImageTag.ImageName,
					promoteMe.Digest,
					promoteMe.DstImageTag.Tag,
					tp)
			}

			// Save some information about this request. It's a bit like
			// HTTP "headers".
			req.RequestParams = PromotionRequest{
				tp,
				// TODO: Clean up types to avoid having to split up promoteMe
				// prematurely like this.
				promoteMe.SrcRegistry.Name,
				promoteMe.DstRegistry.Name,
				promoteMe.DstRegistry.ServiceAccount,
				promoteMe.SrcImageTag.ImageName,
				promoteMe.DstImageTag.ImageName,
				promoteMe.Digest,
				oldDigest,
				promoteMe.DstImageTag.Tag,
			}
			wg.Add(1)
			reqs <- req

		}
	}
}

// FilterPromotionEdges generates all "edges" that we want to promote.
func (sc *SyncContext) FilterPromotionEdges(
	edges map[PromotionEdge]interface{},
	readRepos bool,
) map[PromotionEdge]interface{} {

	if readRepos {
		regs := getRegistriesToRead(edges)
		for _, reg := range regs {
			klog.Info("reading this reg:", reg)
		}
		sc.ReadRegistries(
			regs,
			// Do not read these registries recursively, because we already know
			// exactly which repositories to read (getRegistriesToRead()).
			false,
			MkReadRepositoryCmdReal)
	}

	return sc.getPromotionCandidates(edges)
}

// EdgesToRegInvImage takes the destination endpoints of all edges and converts
// their information to a RegInvImage type. It uses only those edges that are
// trying to promote to the given destination registry.
func EdgesToRegInvImage(
	edges map[PromotionEdge]interface{},
	destRegistry string) RegInvImage {

	rii := make(RegInvImage)

	destRegistry = strings.TrimRight(destRegistry, "/")

	for edge := range edges {
		imgName := ""
		prefix := ""
		if strings.HasPrefix(
			string(edge.DstRegistry.Name),
			destRegistry) {

			prefix = strings.TrimPrefix(
				string(edge.DstRegistry.Name),
				destRegistry)

			if len(prefix) > 0 {
				imgName = prefix + "/" + string(edge.DstImageTag.ImageName)
			} else {
				imgName = string(edge.DstImageTag.ImageName)
			}

			imgName = strings.TrimLeft(imgName, "/")

		} else {
			continue
		}

		if rii[ImageName(imgName)] == nil {
			rii[ImageName(imgName)] = make(DigestTags)
		}

		digestTags := rii[ImageName(imgName)]
		if len(edge.DstImageTag.Tag) > 0 {
			digestTags[edge.Digest] = append(
				digestTags[edge.Digest],
				edge.DstImageTag.Tag)
		} else {
			digestTags[edge.Digest] = TagSlice{}
		}
	}

	return rii
}

// getRegistriesToRead collects all unique Docker repositories we want to read
// from. This way, we don't have to read the entire Docker registry, but only
// those paths that we are thinking of modifying.
func getRegistriesToRead(
	edges map[PromotionEdge]interface{}) []RegistryContext {

	rcs := make(map[RegistryContext]interface{})

	// Save the src and dst endpoints as registries. We only care about the
	// registry and image name, not the tag or digest; this is to collect all
	// unique Docker repositories that we care about.
	for edge := range edges {
		srcReg := edge.SrcRegistry
		srcReg.Name = srcReg.Name +
			"/" +
			RegistryName(edge.SrcImageTag.ImageName)

		rcs[srcReg] = nil

		dstReg := edge.DstRegistry
		dstReg.Name = dstReg.Name +
			"/" +
			RegistryName(edge.DstImageTag.ImageName)

		rcs[dstReg] = nil
	}

	rcsFinal := []RegistryContext{}
	for rc := range rcs {
		rcsFinal = append(rcsFinal, rc)
	}

	return rcsFinal
}

// Promote perferms container image promotion by realizing the intent in the
// Manifest.
//
// nolint[gocyclo]
func (sc *SyncContext) Promote(
	edges map[PromotionEdge]interface{},
	mkProducer func(
		RegistryName,
		ImageName,
		RegistryContext,
		ImageName,
		Digest,
		Tag,
		TagOp) stream.Producer,
	customProcessRequest *ProcessRequest) error {

	if len(edges) == 0 {
		klog.Info("Nothing to promote.")
		return nil
	}

	klog.Info("Pending promotions:")
	for edge := range edges {
		klog.Infof("  %v\n", edge)
	}

	var populateRequests = mkPopulateRequestsForPromotionEdges(
		edges,
		mkProducer)

	var processRequest ProcessRequest
	var processRequestReal ProcessRequest = func(
		sc *SyncContext,
		reqs chan stream.ExternalRequest,
		requestResults chan<- RequestResult,
		wg *sync.WaitGroup,
		mutex *sync.Mutex) {

		for req := range reqs {
			reqRes := RequestResult{Context: req}
			errors := make(Errors, 0)
			// If we're adding or moving (i.e., creating a new image or
			// overwriting), do not bother shelling out to gcloud. Instead just
			// use the gcrane.doCopy() method directly.

			var err error
			var stdoutReader io.Reader
			var stderrReader io.Reader

			rpr := req.RequestParams.(PromotionRequest)
			if rpr.TagOp == Move || rpr.TagOp == Add {

				srcVertex := ToFQIN(rpr.RegistrySrc, rpr.ImageNameSrc, rpr.Digest)

				var dstVertex string

				if len(rpr.Tag) > 0 {
					dstVertex = ToPQIN(
						rpr.RegistryDest,
						rpr.ImageNameDest,
						rpr.Tag)
				} else {
					// If there is no tag, then it is a tagless promotion. So
					// the destination vertex must be referenced with a digest
					// (FQIN), not a tag (PQIN).
					dstVertex = ToFQIN(
						rpr.RegistryDest,
						rpr.ImageNameDest,
						rpr.Digest)
				}

				if err := crane.Copy(srcVertex, dstVertex); err != nil {
					klog.Error(err)
					errors = append(errors, Error{
						Context: "running writeImage()",
						Error:   err})
				}

			} else {
				stdoutReader, stderrReader, err = req.StreamProducer.Produce()
				if err != nil {
					errors = append(errors, Error{
						Context: "running process",
						Error:   err})
				}
				b, err := ioutil.ReadAll(stdoutReader)
				if err != nil {
					errors = append(errors, Error{
						Context: "reading process stdout",
						Error:   err})
				}
				be, err := ioutil.ReadAll(stderrReader)
				if err != nil {
					errors = append(errors, Error{
						Context: "reading process stderr",
						Error:   err})
				}
				// The add-tag has stderr; it uses stderr for debug messages, so
				// don't count it as an error. Instead just print it out as extra
				// info.
				klog.Infof("process stdout:\n%v\n", string(b))
				klog.Infof("process stderr:\n%v\n", string(be))
				err = req.StreamProducer.Close()
				if err != nil {
					errors = append(errors, Error{
						Context: "closing process",
						Error:   err})
				}
			}
			reqRes.Errors = errors
			requestResults <- reqRes
			wg.Add(-1)
		}
	}

	captured := make(CapturedRequests)

	if sc.DryRun {
		processRequestDryRun := MkRequestCapturer(&captured)
		processRequest = processRequestDryRun
	} else {
		processRequest = processRequestReal
	}

	if customProcessRequest != nil {
		processRequest = *customProcessRequest
	}
	sc.ExecRequests(populateRequests, processRequest)

	if sc.DryRun {
		sc.PrintCapturedRequests(&captured)
	}

	return nil
}

// PrintCapturedRequests pretty-prints all given PromotionRequests.
func (sc *SyncContext) PrintCapturedRequests(capReqs *CapturedRequests) {
	prs := make([]PromotionRequest, 0)

	for req, count := range *capReqs {
		for i := 0; i < count; i++ {
			prs = append(prs, req)
		}
	}

	sort.Slice(prs, func(i, j int) bool {
		return prs[i].PrettyValue() < prs[j].PrettyValue()
	})
	if len(prs) > 0 {
		fmt.Println("")
		fmt.Println("captured reqs summary:")
		fmt.Println("")
		for _, pr := range prs {
			fmt.Printf("captured req: %v", pr.PrettyValue())
		}
		fmt.Println("")
	} else {
		fmt.Println("No requests captured.")
	}
}

// PrettyValue is a prettified string representation of a TagOp.
func (op *TagOp) PrettyValue() string {
	var tagOpPretty string
	switch *op {
	case Add:
		tagOpPretty = "ADD"
	case Move:
		tagOpPretty = "MOVE"
	case Delete:
		tagOpPretty = "DELETE"
	}
	return tagOpPretty
}

// PrettyValue is a prettified string representation of a PromotionRequest.
func (pr *PromotionRequest) PrettyValue() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%v -> %v: Tag: '%v' <%v> %v",
		ToLQIN(pr.RegistrySrc, pr.ImageNameSrc),
		ToLQIN(pr.RegistryDest, pr.ImageNameDest),
		string(pr.Tag),
		pr.TagOp.PrettyValue(),
		string(pr.Digest))
	if len(pr.DigestOld) > 0 {
		fmt.Fprintf(&b, " (move from '%v')", string(pr.DigestOld))
	}
	fmt.Fprintf(&b, "\n")

	return b.String()
}

// MkRequestCapturer returns a function that simply records requests as they are
// captured (slured out from the reqs channel).
func MkRequestCapturer(captured *CapturedRequests) ProcessRequest {
	return func(
		sc *SyncContext,
		reqs chan stream.ExternalRequest,
		errs chan<- RequestResult,
		wg *sync.WaitGroup,
		mutex *sync.Mutex) {

		for req := range reqs {
			pr := req.RequestParams.(PromotionRequest)
			mutex.Lock()
			if _, ok := (*captured)[pr]; ok {
				(*captured)[pr]++
			} else {
				(*captured)[pr] = 1
			}
			mutex.Unlock()
			wg.Add(-1)
		}
	}
}

// GarbageCollect deletes all images that are not referenced by Docker tags.
// nolint[gocyclo]
func (sc *SyncContext) GarbageCollect(
	mfest Manifest,
	mkProducer func(RegistryContext, ImageName, Digest) stream.Producer,
	customProcessRequest *ProcessRequest) {

	var populateRequests PopulateRequests = func(
		sc *SyncContext,
		reqs chan<- stream.ExternalRequest,
		wg *sync.WaitGroup) {

		for _, registry := range mfest.Registries {
			if registry.Name == sc.SrcRegistry.Name {
				continue
			}
			for imageName, digestTags := range sc.Inv[registry.Name] {
				for digest, tagArray := range digestTags {
					if len(tagArray) == 0 {
						var req stream.ExternalRequest
						req.StreamProducer = mkProducer(
							registry,
							imageName,
							digest)
						req.RequestParams = PromotionRequest{
							Delete,
							sc.SrcRegistry.Name,
							registry.Name,
							registry.ServiceAccount,

							// No source image name, because tag deletions
							// should only delete the what's in the
							// destination registry
							ImageName(""),

							imageName,
							digest,
							"",
							"",
						}
						wg.Add(1)
						reqs <- req
					}
				}
			}
		}
	}

	var processRequest ProcessRequest
	var processRequestReal ProcessRequest = func(
		sc *SyncContext,
		reqs chan stream.ExternalRequest,
		requestResults chan<- RequestResult,
		wg *sync.WaitGroup,
		mutex *sync.Mutex) {

		for req := range reqs {
			reqRes := RequestResult{Context: req}
			jsons, errors := getJSONSFromProcess(req)
			if len(errors) > 0 {
				reqRes.Errors = errors
				requestResults <- reqRes
				continue
			}
			for _, json := range jsons {
				klog.Info("DELETED image:", json)
			}
			reqRes.Errors = errors
			requestResults <- reqRes
			wg.Add(-1)
		}
	}

	captured := make(CapturedRequests)

	if sc.DryRun {
		processRequestDryRun := MkRequestCapturer(&captured)
		processRequest = processRequestDryRun
	} else {
		processRequest = processRequestReal
	}

	if customProcessRequest != nil {
		processRequest = *customProcessRequest
	}
	sc.ExecRequests(populateRequests, processRequest)

	if sc.DryRun {
		sc.PrintCapturedRequests(&captured)
	}
}

func supportedMediaType(v string) (cr.MediaType, error) {
	switch cr.MediaType(v) {
	case cr.DockerManifestList:
		return cr.DockerManifestList, nil
	case cr.DockerManifestSchema1:
		return cr.DockerManifestSchema1, nil
	case cr.DockerManifestSchema1Signed:
		return cr.DockerManifestSchema1Signed, nil
	case cr.DockerManifestSchema2:
		return cr.DockerManifestSchema2, nil
	default:
		return cr.MediaType(""), fmt.Errorf("unsupported MediaType %s", v)
	}
}

// ClearRepository wipes out all Docker images from a registry! Use with caution.
// nolint[gocyclo]
//
// TODO: Maybe split this into 2 parts, so that each part can be unit-tested
// separately (deletion of manifest lists vs deletion of other media types).
func (sc *SyncContext) ClearRepository(
	regName RegistryName,
	mkProducer func(RegistryContext, ImageName, Digest) stream.Producer,
	customProcessRequest *ProcessRequest) {

	// deleteRequestsPopulator returns a PopulateRequests that
	// varies by a predicate. Closure city!
	var deleteRequestsPopulator func(func(cr.MediaType) bool) PopulateRequests = func(predicate func(cr.MediaType) bool) PopulateRequests {

		var populateRequests PopulateRequests = func(
			sc *SyncContext,
			reqs chan<- stream.ExternalRequest,
			wg *sync.WaitGroup) {

			for _, registry := range sc.RegistryContexts {
				// Skip over any registry that does not match the regName we want to
				// wipe.
				if registry.Name != regName {
					continue
				}
				for imageName, digestTags := range sc.Inv[registry.Name] {
					for digest, _ := range digestTags {
						mediaType, ok := sc.DigestMediaType[digest]
						if !ok {
							fmt.Println("could not detect MediaType of digest", digest)
							continue
						}
						if !predicate(mediaType) {
							fmt.Printf("skipping digest %s mediaType %s\n", digest, mediaType)
							continue
						}
						var req stream.ExternalRequest
						req.StreamProducer = mkProducer(
							registry,
							imageName,
							digest)
						req.RequestParams = PromotionRequest{
							Delete,
							"",
							registry.Name,
							registry.ServiceAccount,

							// No source image name, because tag deletions
							// should only delete the what's in the
							// destination registry
							ImageName(""),

							imageName,
							digest,
							"",
							"",
						}
						wg.Add(1)
						reqs <- req
					}
				}
			}
		}
		return populateRequests
	}

	var processRequest ProcessRequest
	var processRequestReal ProcessRequest = func(
		sc *SyncContext,
		reqs chan stream.ExternalRequest,
		requestResults chan<- RequestResult,
		wg *sync.WaitGroup,
		mutex *sync.Mutex) {

		for req := range reqs {
			reqRes := RequestResult{Context: req}
			jsons, errors := getJSONSFromProcess(req)
			if len(errors) > 0 {
				reqRes.Errors = errors
				requestResults <- reqRes
				// Don't skip over this request, because stderr here is ignorable.
				// continue
			}
			for _, json := range jsons {
				klog.Info("DELETED image:", json)
			}
			reqRes.Errors = errors
			requestResults <- reqRes
			wg.Add(-1)
		}
	}

	captured := make(CapturedRequests)

	if sc.DryRun {
		processRequestDryRun := MkRequestCapturer(&captured)
		processRequest = processRequestDryRun
	} else {
		processRequest = processRequestReal
	}

	if customProcessRequest != nil {
		processRequest = *customProcessRequest
	}

	var isEqualTo (func(cr.MediaType) func(cr.MediaType) bool) = func(want cr.MediaType) func(cr.MediaType) bool {
		return func(got cr.MediaType) bool {
			return want == got
		}
	}

	var isNotEqualTo (func(cr.MediaType) func(cr.MediaType) bool) = func(want cr.MediaType) func(cr.MediaType) bool {
		return func(got cr.MediaType) bool {
			return want != got
		}
	}

	// Avoid the GCR error that complains if you try to delete an image which is
	// referenced by a DockerManifestList, by first deleting all such manifest
	// lists.
	deleteManifestLists := deleteRequestsPopulator(isEqualTo(cr.DockerManifestList))
	sc.ExecRequests(deleteManifestLists, processRequest)
	deleteOthers := deleteRequestsPopulator(isNotEqualTo(cr.DockerManifestList))
	sc.ExecRequests(deleteOthers, processRequest)

	if sc.DryRun {
		sc.PrintCapturedRequests(&captured)
	}
}

// GetWriteCmd generates a gcloud command that is used to make modifications to
// a Docker Registry.
func GetWriteCmd(
	dest RegistryContext,
	useServiceAccount bool,
	srcRegistry RegistryName,
	srcImageName ImageName,
	destImageName ImageName,
	digest Digest,
	tag Tag,
	tp TagOp) []string {

	var cmd []string
	switch tp {
	case Delete:
		cmd = []string{"gcloud",
			"--quiet",
			"container",
			"images",
			"untag",
			ToPQIN(dest.Name, destImageName, tag)}
	default:
		klog.Exitln("unsupported tag operation:", tp)
	}
	// Use the service account if it is desired.
	return gcloud.MaybeUseServiceAccount(
		dest.ServiceAccount,
		useServiceAccount,
		cmd)
}

// GetDeleteCmd generates the cloud command used to delete images (used for
// garbage collection).
func GetDeleteCmd(
	rc RegistryContext,
	useServiceAccount bool,
	img ImageName,
	digest Digest,
	force bool) []string {

	fqin := ToFQIN(rc.Name, img, digest)
	cmd := []string{
		"gcloud",
		"container",
		"images",
		"delete",
		fqin,
		"--format=json"}
	if force {
		cmd = append(cmd, "--force-delete-tags")
		cmd = append(cmd, "--quiet")
	}
	return gcloud.MaybeUseServiceAccount(
		rc.ServiceAccount,
		useServiceAccount,
		cmd)
}
