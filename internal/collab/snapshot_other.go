//go:build !unix

package collab

func readReviewFile(root, path string) (FileImage, error) { return FileImage{}, ErrUnavailable }
