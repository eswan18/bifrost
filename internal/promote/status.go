package promote

type State string

const (
	InSync    State = "in_sync"
	OutOfSync State = "out_of_sync"
	MidDeploy State = "mid_deploy"
	Unknown   State = "unknown"
)

type Status struct {
	State      State
	StagingTag string
	ProdTag    string
	// NewProdTag is set only when OutOfSync — it's what would be applied to
	// prod by a promote.
	NewProdTag string
}

// StatusOf compares the set of images deployed in staging and prod. Images
// are full image URLs (e.g. "registry/foo:abc123"). Returns the derived
// Status. Mirrors ib.py:88-163.
func StatusOf(stagingImages, prodImages []string) Status {
	if len(stagingImages) == 0 || len(prodImages) == 0 {
		return Status{State: Unknown}
	}
	if dedupe(stagingImages) > 1 || dedupe(prodImages) > 1 {
		return Status{State: MidDeploy}
	}

	stagingTag := ExtractTag(stagingImages[0])
	prodTag := ExtractTag(prodImages[0])
	stagingSHA := ExtractSHA(stagingTag)
	prodSHA := ExtractSHA(prodTag)

	if stagingSHA == "" || prodSHA == "" {
		return Status{State: Unknown, StagingTag: stagingTag, ProdTag: prodTag}
	}

	if stagingSHA == prodSHA {
		return Status{State: InSync, StagingTag: stagingTag, ProdTag: prodTag}
	}
	return Status{
		State:      OutOfSync,
		StagingTag: stagingTag,
		ProdTag:    prodTag,
		NewProdTag: NewProdTag(stagingSHA, stagingTag, prodTag),
	}
}

func dedupe(s []string) int {
	seen := map[string]struct{}{}
	for _, v := range s {
		seen[v] = struct{}{}
	}
	return len(seen)
}
