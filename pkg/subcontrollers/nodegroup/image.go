package nodegroup

import (
	"fmt"
	"strings"

	srapi "github.com/StarRocks/starrocks-kubernetes-operator/pkg/apis/starrocks/v1"
)

// ResolveImage returns the image the pods of a node group run.
//
// A group never chooses its own version: the tag is always the one the cluster runs, so that changing the
// image of the cluster is the only way to upgrade its groups.
//   - spec.template.image empty: the group runs the image of the cluster's component of its role.
//   - spec.template.image set: it names a repository only, and the tag of the cluster's component is appended.
//     A CN group of a cluster without starRocksCnSpec takes the tag of the FE image.
func ResolveImage(cluster *srapi.StarRocksCluster, group *srapi.StarRocksNodeGroup) (string, error) {
	if cluster.Spec.StarRocksFeSpec == nil {
		return "", fmt.Errorf("cluster %s has no starRocksFeSpec", cluster.Name)
	}

	clusterImage := cluster.Spec.StarRocksFeSpec.Image
	switch group.Spec.Role {
	case srapi.NodeGroupRoleFEObserver:
	case srapi.NodeGroupRoleCN:
		if cluster.Spec.StarRocksCnSpec != nil {
			clusterImage = cluster.Spec.StarRocksCnSpec.Image
		} else if group.Spec.Template.Image == "" {
			return "", fmt.Errorf("cluster %s has no starRocksCnSpec to take the CN image from: "+
				"set spec.template.image to the repository of the CN image, without a tag", cluster.Name)
		}
	default:
		return "", fmt.Errorf("unknown role %q", group.Spec.Role)
	}

	repository := group.Spec.Template.Image
	if repository == "" {
		return clusterImage, nil
	}
	if tag, digest := imageTag(repository); tag != "" || digest {
		return "", fmt.Errorf("spec.template.image %q must name a repository only: "+
			"the tag is always taken from cluster %s", repository, cluster.Name)
	}

	tag, digest := imageTag(clusterImage)
	if digest || tag == "" {
		return "", fmt.Errorf("image %q of cluster %s has no tag to share with the node group: "+
			"leave spec.template.image empty to run that image as it is", clusterImage, cluster.Name)
	}
	return repository + ":" + tag, nil
}

// imageTag returns the tag of an image reference. digest reports a reference pinned by digest, which has no
// tag to speak of.
func imageTag(image string) (tag string, digest bool) {
	if strings.Contains(image, "@") {
		return "", true
	}
	// a colon before the last slash belongs to the registry port, not to a tag
	lastSlash := strings.LastIndex(image, "/")
	if colon := strings.LastIndex(image, ":"); colon > lastSlash {
		return image[colon+1:], false
	}
	return "", false
}
