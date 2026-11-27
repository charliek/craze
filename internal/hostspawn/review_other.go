//go:build !linux && !darwin

package hostspawn

func enableReviewAdoption(bool) (bool, error) { return false, nil }
func reviewChildrenGone() (bool, error)       { return true, nil }
func reviewParentVersion(int) int32           { return 0 }
func reviewOlderParent(int, int32) bool       { return false }
