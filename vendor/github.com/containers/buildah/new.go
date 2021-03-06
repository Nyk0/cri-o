package buildah

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/containers/buildah/util"
	"github.com/containers/image/v5/image"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/image/v5/pkg/sysregistriesv2"
	is "github.com/containers/image/v5/storage"
	"github.com/containers/image/v5/transports"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/image/v5/types"
	"github.com/containers/storage"
	digest "github.com/opencontainers/go-digest"
	"github.com/openshift/imagebuilder"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	// BaseImageFakeName is the "name" of a source image which we interpret
	// as "no image".
	BaseImageFakeName = imagebuilder.NoBaseImageSpecifier
)

func pullAndFindImage(ctx context.Context, store storage.Store, srcRef types.ImageReference, options BuilderOptions, sc *types.SystemContext) (*storage.Image, types.ImageReference, error) {
	pullOptions := PullOptions{
		ReportWriter:  options.ReportWriter,
		Store:         store,
		SystemContext: options.SystemContext,
		BlobDirectory: options.BlobDirectory,
		MaxRetries:    options.MaxPullRetries,
		RetryDelay:    options.PullRetryDelay,
	}
	ref, err := pullImage(ctx, store, srcRef, pullOptions, sc)
	if err != nil {
		logrus.Debugf("error pulling image %q: %v", transports.ImageName(srcRef), err)
		return nil, nil, err
	}
	img, err := is.Transport.GetStoreImage(store, ref)
	if err != nil {
		logrus.Debugf("error reading pulled image %q: %v", transports.ImageName(srcRef), err)
		return nil, nil, errors.Wrapf(err, "error locating image %q in local storage", transports.ImageName(ref))
	}
	return img, ref, nil
}

func getImageName(name string, img *storage.Image) string {
	imageName := name
	if len(img.Names) > 0 {
		imageName = img.Names[0]
		// When the image used by the container is a tagged image
		// the container name might be set to the original image instead of
		// the image given in the "from" command line.
		// This loop is supposed to fix this.
		for _, n := range img.Names {
			if strings.Contains(n, name) {
				imageName = n
				break
			}
		}
	}
	return imageName
}

func imageNamePrefix(imageName string) string {
	prefix := imageName
	s := strings.Split(prefix, ":")
	if len(s) > 0 {
		prefix = s[0]
	}
	s = strings.Split(prefix, "/")
	if len(s) > 0 {
		prefix = s[len(s)-1]
	}
	s = strings.Split(prefix, "@")
	if len(s) > 0 {
		prefix = s[0]
	}
	return prefix
}

func newContainerIDMappingOptions(idmapOptions *IDMappingOptions) storage.IDMappingOptions {
	var options storage.IDMappingOptions
	if idmapOptions != nil {
		options.HostUIDMapping = idmapOptions.HostUIDMapping
		options.HostGIDMapping = idmapOptions.HostGIDMapping
		uidmap, gidmap := convertRuntimeIDMaps(idmapOptions.UIDMap, idmapOptions.GIDMap)
		if len(uidmap) > 0 && len(gidmap) > 0 {
			options.UIDMap = uidmap
			options.GIDMap = gidmap
		} else {
			options.HostUIDMapping = true
			options.HostGIDMapping = true
		}
	}
	return options
}

func resolveImage(ctx context.Context, systemContext *types.SystemContext, store storage.Store, options BuilderOptions) (types.ImageReference, string, *storage.Image, error) {
	type failure struct {
		resolvedImageName string
		err               error
	}
	candidates, transport, searchRegistriesWereUsedButEmpty, err := util.ResolveName(options.FromImage, options.Registry, systemContext, store)
	if err != nil {
		return nil, "", nil, errors.Wrapf(err, "error parsing reference to image %q", options.FromImage)
	}

	failures := []failure{}
	for _, image := range candidates {
		if transport == "" {
			img, err := store.Image(image)
			if err != nil {
				logrus.Debugf("error looking up known-local image %q: %v", image, err)
				failures = append(failures, failure{resolvedImageName: image, err: err})
				continue
			}
			ref, err := is.Transport.ParseStoreReference(store, img.ID)
			if err != nil {
				return nil, "", nil, errors.Wrapf(err, "error parsing reference to image %q", img.ID)
			}
			return ref, transport, img, nil
		}

		trans := transport
		if transport != util.DefaultTransport {
			trans = trans + ":"
		}
		srcRef, err := alltransports.ParseImageName(trans + image)
		if err != nil {
			logrus.Debugf("error parsing image name %q: %v", trans+image, err)
			failures = append(failures, failure{
				resolvedImageName: image,
				err:               errors.Wrapf(err, "error parsing attempted image name %q", trans+image),
			})
			continue
		}

		if options.PullPolicy == PullAlways {
			pulledImg, pulledReference, err := pullAndFindImage(ctx, store, srcRef, options, systemContext)
			if err != nil {
				logrus.Debugf("unable to pull and read image %q: %v", image, err)
				failures = append(failures, failure{resolvedImageName: image, err: err})
				continue
			}
			return pulledReference, transport, pulledImg, nil
		}

		destImage, err := localImageNameForReference(ctx, store, srcRef)
		if err != nil {
			return nil, "", nil, errors.Wrapf(err, "error computing local image name for %q", transports.ImageName(srcRef))
		}
		if destImage == "" {
			return nil, "", nil, errors.Errorf("error computing local image name for %q", transports.ImageName(srcRef))
		}
		ref, err := is.Transport.ParseStoreReference(store, destImage)
		if err != nil {
			return nil, "", nil, errors.Wrapf(err, "error parsing reference to image %q", destImage)
		}

		if options.PullPolicy == PullIfNewer {
			img, err := is.Transport.GetStoreImage(store, ref)
			if err == nil {
				// Let's see if this image is on the repository and if it's there
				// then note it's Created date.
				var repoImageCreated time.Time
				repoImageFound := false
				repoImage, err := srcRef.NewImage(ctx, systemContext)
				if err == nil {
					inspect, err := repoImage.Inspect(ctx)
					if err == nil {
						repoImageFound = true
						repoImageCreated = *inspect.Created
					}
					repoImage.Close()
				}
				if !repoImageFound || repoImageCreated == img.Created {
					// The image is only local or the same date is on the
					// local and repo versions of the image, no need to pull.
					return ref, transport, img, nil
				}
			}
		} else {
			// Get the image from the store if present for PullNever and PullIfMissing
			img, err := is.Transport.GetStoreImage(store, ref)
			if err == nil {
				return ref, transport, img, nil
			}
			if errors.Cause(err) == storage.ErrImageUnknown && options.PullPolicy == PullNever {
				logrus.Debugf("no such image %q: %v", transports.ImageName(ref), err)
				failures = append(failures, failure{
					resolvedImageName: image,
					err:               errors.Errorf("no such image %q", transports.ImageName(ref)),
				})
				continue
			}
		}

		pulledImg, pulledReference, err := pullAndFindImage(ctx, store, srcRef, options, systemContext)
		if err != nil {
			logrus.Debugf("unable to pull and read image %q: %v", image, err)
			failures = append(failures, failure{resolvedImageName: image, err: err})
			continue
		}
		return pulledReference, transport, pulledImg, nil
	}

	if len(failures) != len(candidates) {
		return nil, "", nil, errors.Errorf("internal error: %d candidates (%#v) vs. %d failures (%#v)", len(candidates), candidates, len(failures), failures)
	}

	registriesConfPath := sysregistriesv2.ConfigPath(systemContext)
	switch len(failures) {
	case 0:
		if searchRegistriesWereUsedButEmpty {
			return nil, "", nil, errors.Errorf("image name %q is a short name and no search registries are defined in %s.", options.FromImage, registriesConfPath)
		}
		return nil, "", nil, errors.Errorf("internal error: no pull candidates were available for %q for an unknown reason", options.FromImage)

	case 1:
		err := failures[0].err
		if failures[0].resolvedImageName != options.FromImage {
			err = errors.Wrapf(err, "while pulling %q as %q", options.FromImage, failures[0].resolvedImageName)
		}
		if searchRegistriesWereUsedButEmpty {
			err = errors.Wrapf(err, "(image name %q is a short name and no search registries are defined in %s)", options.FromImage, registriesConfPath)
		}
		return nil, "", nil, err

	default:
		// NOTE: a multi-line error string:
		e := fmt.Sprintf("The following failures happened while trying to pull image specified by %q based on search registries in %s:", options.FromImage, registriesConfPath)
		for _, f := range failures {
			e = e + fmt.Sprintf("\n* %q: %s", f.resolvedImageName, f.err.Error())
		}
		return nil, "", nil, errors.New(e)
	}
}

func containerNameExist(name string, containers []storage.Container) bool {
	for _, container := range containers {
		for _, cname := range container.Names {
			if cname == name {
				return true
			}
		}
	}
	return false
}

func findUnusedContainer(name string, containers []storage.Container) string {
	suffix := 1
	tmpName := name
	for containerNameExist(tmpName, containers) {
		tmpName = fmt.Sprintf("%s-%d", name, suffix)
		suffix++
	}
	return tmpName
}

func newBuilder(ctx context.Context, store storage.Store, options BuilderOptions) (*Builder, error) {
	var (
		ref types.ImageReference
		img *storage.Image
		err error
	)
	if options.FromImage == BaseImageFakeName {
		options.FromImage = ""
	}

	systemContext := getSystemContext(store, options.SystemContext, options.SignaturePolicyPath)

	if options.FromImage != "" && options.FromImage != "scratch" {
		ref, _, img, err = resolveImage(ctx, systemContext, store, options)
		if err != nil {
			return nil, err
		}
	}
	imageSpec := options.FromImage
	imageID := ""
	imageDigest := ""
	topLayer := ""
	if img != nil {
		imageSpec = getImageName(imageNamePrefix(imageSpec), img)
		imageID = img.ID
		topLayer = img.TopLayer
	}
	var src types.Image
	if ref != nil {
		srcSrc, err := ref.NewImageSource(ctx, systemContext)
		if err != nil {
			return nil, errors.Wrapf(err, "error instantiating image for %q", transports.ImageName(ref))
		}
		defer srcSrc.Close()
		manifestBytes, manifestType, err := srcSrc.GetManifest(ctx, nil)
		if err != nil {
			return nil, errors.Wrapf(err, "error loading image manifest for %q", transports.ImageName(ref))
		}
		if manifestDigest, err := manifest.Digest(manifestBytes); err == nil {
			imageDigest = manifestDigest.String()
		}
		var instanceDigest *digest.Digest
		if manifest.MIMETypeIsMultiImage(manifestType) {
			list, err := manifest.ListFromBlob(manifestBytes, manifestType)
			if err != nil {
				return nil, errors.Wrapf(err, "error parsing image manifest for %q as list", transports.ImageName(ref))
			}
			instance, err := list.ChooseInstance(systemContext)
			if err != nil {
				return nil, errors.Wrapf(err, "error finding an appropriate image in manifest list %q", transports.ImageName(ref))
			}
			instanceDigest = &instance
		}
		src, err = image.FromUnparsedImage(ctx, systemContext, image.UnparsedInstance(srcSrc, instanceDigest))
		if err != nil {
			return nil, errors.Wrapf(err, "error instantiating image for %q instance %q", transports.ImageName(ref), instanceDigest)
		}
	}

	name := "working-container"
	if options.Container != "" {
		name = options.Container
	} else {
		if imageSpec != "" {
			name = imageNamePrefix(imageSpec) + "-" + name
		}
	}
	var container *storage.Container
	tmpName := name
	if options.Container == "" {
		containers, err := store.Containers()
		if err != nil {
			return nil, errors.Wrapf(err, "unable to check for container names")
		}
		tmpName = findUnusedContainer(tmpName, containers)
	}

	conflict := 100
	for {
		coptions := storage.ContainerOptions{
			LabelOpts:        options.CommonBuildOpts.LabelOpts,
			IDMappingOptions: newContainerIDMappingOptions(options.IDMappingOptions),
		}
		container, err = store.CreateContainer("", []string{tmpName}, imageID, "", "", &coptions)
		if err == nil {
			name = tmpName
			break
		}
		if errors.Cause(err) != storage.ErrDuplicateName || options.Container != "" {
			return nil, errors.Wrapf(err, "error creating container")
		}
		tmpName = fmt.Sprintf("%s-%d", name, rand.Int()%conflict)
		conflict = conflict * 10
	}
	defer func() {
		if err != nil {
			if err2 := store.DeleteContainer(container.ID); err2 != nil {
				logrus.Errorf("error deleting container %q: %v", container.ID, err2)
			}
		}
	}()

	uidmap, gidmap := convertStorageIDMaps(container.UIDMap, container.GIDMap)

	defaultNamespaceOptions, err := DefaultNamespaceOptions()
	if err != nil {
		return nil, err
	}

	namespaceOptions := defaultNamespaceOptions
	namespaceOptions.AddOrReplace(options.NamespaceOptions...)

	builder := &Builder{
		store:                 store,
		Type:                  containerType,
		FromImage:             imageSpec,
		FromImageID:           imageID,
		FromImageDigest:       imageDigest,
		Container:             name,
		ContainerID:           container.ID,
		ImageAnnotations:      map[string]string{},
		ImageCreatedBy:        "",
		ProcessLabel:          container.ProcessLabel(),
		MountLabel:            container.MountLabel(),
		DefaultMountsFilePath: options.DefaultMountsFilePath,
		Isolation:             options.Isolation,
		NamespaceOptions:      namespaceOptions,
		ConfigureNetwork:      options.ConfigureNetwork,
		CNIPluginPath:         options.CNIPluginPath,
		CNIConfigDir:          options.CNIConfigDir,
		IDMappingOptions: IDMappingOptions{
			HostUIDMapping: len(uidmap) == 0,
			HostGIDMapping: len(uidmap) == 0,
			UIDMap:         uidmap,
			GIDMap:         gidmap,
		},
		Capabilities:    copyStringSlice(options.Capabilities),
		CommonBuildOpts: options.CommonBuildOpts,
		TopLayer:        topLayer,
		Args:            options.Args,
		Format:          options.Format,
		TempVolumes:     map[string]bool{},
		Devices:         options.Devices,
	}

	if options.Mount {
		_, err = builder.Mount(container.MountLabel())
		if err != nil {
			return nil, errors.Wrapf(err, "error mounting build container %q", builder.ContainerID)
		}
	}

	if err := builder.initConfig(ctx, src); err != nil {
		return nil, errors.Wrapf(err, "error preparing image configuration")
	}
	err = builder.Save()
	if err != nil {
		return nil, errors.Wrapf(err, "error saving builder state for container %q", builder.ContainerID)
	}

	return builder, nil
}
