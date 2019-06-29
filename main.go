package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"github.com/PuerkitoBio/goquery"
	"github.com/hashicorp/go-version"
	"github.com/hashicorp/terraform/config"
)

type tfVersion struct {
	Version              *version.Version
	URL                  *url.URL
	ChecksumURL          *url.URL
	ChecksumSignatureURL *url.URL
}

var (
	baseURL           *url.URL
	dataDirPath       string
	tfVersionsDirPath string
	cacheDirPath      string
)

func init() {
	_baseURL, err := url.Parse("https://releases.hashicorp.com/terraform/")

	if err != nil {
		log.Fatal(err)
	}

	baseURL = _baseURL

	userHomeDirPath, err := os.UserHomeDir()

	if err != nil {
		log.Fatal(err)
	}

	dataDirPath = path.Join(userHomeDirPath, ".local/share/tvm")

	if _, err := os.Stat(dataDirPath); os.IsNotExist(err) {
		err = os.Mkdir(dataDirPath, 0755)

		if err != nil {
			log.Fatal(err)
		}
	}

	tfVersionsDirPath = path.Join(dataDirPath, "versions")

	if _, err := os.Stat(tfVersionsDirPath); os.IsNotExist(err) {
		err = os.Mkdir(tfVersionsDirPath, 0755)

		if err != nil {
			log.Fatal(err)
		}
	}

	userCacheDirPath, err := os.UserCacheDir()

	if err != nil {
		log.Fatal(err)
	}

	cacheDirPath = path.Join(userCacheDirPath, "tvm")

	if _, err := os.Stat(cacheDirPath); os.IsNotExist(err) {
		err = os.Mkdir(cacheDirPath, 0755)

		if err != nil {
			log.Fatal(err)
		}
	}
}

func main() {
	listCmd := flag.NewFlagSet("list", flag.ExitOnError)
	installCmd := flag.NewFlagSet("install", flag.ExitOnError)
	execCmd := flag.NewFlagSet("exec", flag.ExitOnError)

	if path.Base(os.Args[0]) == "terraform" {
		exec(os.Args[1:])
	} else if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "list":
			if err := listCmd.Parse(os.Args[2:]); err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			list()
		case "install":
			if err := installCmd.Parse(os.Args[2:]); err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			install()
		case "exec":
			if err := execCmd.Parse(os.Args[2:]); err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			exec(os.Args[2:])
		}
	} else {
		fmt.Println("Too few arguments")
		os.Exit(1)
	}
}

func scrape(url *url.URL) (*goquery.Document, error) {
	resp, err := http.Get(url.String())

	if err != nil {
		return nil, fmt.Errorf("Failed to get %s: %s", url, err)
	}

	defer func() {
		if err := resp.Body.Close(); err != nil {
			fmt.Println("Error closing response body")
		}
	}()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Error getting %s: %s", url, resp.Status)
	}

	return goquery.NewDocumentFromReader(resp.Body)
}

func get() []tfVersion {
	doc, err := scrape(baseURL)

	if err != nil {
		log.Fatal(err)
	}

	urls := make([]*url.URL, 0)

	doc.Find("body ul li a").Each(func(i int, s *goquery.Selection) {
		href, ok := s.Attr("href")

		if !ok {
			return
		}

		url, err := url.Parse(href)

		if err != nil {
			return
		}

		url = baseURL.ResolveReference(url)

		if strings.HasPrefix(url.Path, baseURL.Path) {
			urls = append(urls, url)
		}
	})

	tfVersions := make([]tfVersion, 0)

	c := make(chan tfVersion)

	for _, _url := range urls {
		go func(url *url.URL) {
			doc, err := scrape(url)

			if err != nil {
				log.Fatal(err)
			}

			tfVersion := tfVersion{}

			doc.Find("body ul li a").Each(func(i int, s *goquery.Selection) {
				_url, ok := s.Attr("href")

				if !ok {
					return
				}

				url, err := url.Parse(_url)

				if err != nil {
					return
				}

				if strings.HasSuffix(_url, "_SHA256SUMS") {
					tfVersion.ChecksumURL = url

					return
				}

				if strings.HasSuffix(_url, "_SHA256SUMS.sig") {
					tfVersion.ChecksumSignatureURL = url

					return
				}

				os, ok := s.Attr("data-os")

				if !ok || os != runtime.GOOS {
					return
				}

				arch, ok := s.Attr("data-arch")

				if !ok || arch != runtime.GOARCH {
					return
				}

				_version, ok := s.Attr("data-version")

				if !ok {
					return
				}

				version, err := version.NewVersion(_version)

				if err != nil {
					return
				}

				tfVersion.Version = version
				tfVersion.URL = url
			})

			c <- tfVersion
		}(_url)
	}

	for range urls {
		tfVersion := <-c

		if tfVersion.Version != nil {
			tfVersions = append(tfVersions, tfVersion)
		}
	}

	return tfVersions
}

func sortAsc(tfVersions []tfVersion) []tfVersion {
	sort.Slice(tfVersions, func(i, j int) bool {
		return tfVersions[i].Version.LessThan(tfVersions[j].Version)
	})

	return tfVersions
}

func sortDsc(tfVersions []tfVersion) []tfVersion {
	sort.Slice(tfVersions, func(i, j int) bool {
		return tfVersions[j].Version.LessThan(tfVersions[i].Version)
	})

	return tfVersions
}

func list() {
	tfVersions := sortAsc(get())

	for _, tfVersion := range tfVersions {
		fmt.Println(tfVersion.Version)
	}
}

func getConstraints() version.Constraints {
	currentDir, err := os.Getwd()

	if err != nil {
		log.Fatal(err)
	}

	tfConfig, err := config.LoadDir(currentDir)

	if err != nil {
		log.Fatal(err)
	}

	if tfConfig.Terraform.RequiredVersion == "" {
		return nil
	}

	constraints, err := version.NewConstraint(tfConfig.Terraform.RequiredVersion)

	if err != nil {
		log.Fatal(err)
	}

	return constraints
}

func install() {
	tfVersions := sortDsc(get())

	constraints := getConstraints()

	for _, tfVersion := range tfVersions {
		if constraints.Check(tfVersion.Version) {
			tfVersionDirPath := path.Join(tfVersionsDirPath, tfVersion.Version.String())

			if _, err := os.Stat(tfVersionDirPath); os.IsNotExist(err) {
				err = os.Mkdir(tfVersionDirPath, 0755)

				if err != nil {
					log.Fatal(err)
				}
			}

			archivePath := path.Join(cacheDirPath, path.Base(tfVersion.URL.Path))
			archiveFile, err := os.Create(archivePath)

			if err != nil {
				log.Fatal(err)
			}

			defer func() {
				if err := archiveFile.Close(); err != nil {
					fmt.Println("Error closing file")
				}

				if err := os.Remove(archivePath); err != nil {
					fmt.Println("Error removing file")
				}
			}()

			resp, err := http.Get(tfVersion.URL.String())

			if err != nil {
				log.Fatal(err)
			}

			defer func() {
				if err := resp.Body.Close(); err != nil {
					fmt.Println("Error closing response body")
				}
			}()

			h := sha256.New()

			_, err = io.Copy(archiveFile, io.TeeReader(resp.Body, h))

			if err != nil {
				log.Fatal(err)
			}

			if tfVersion.ChecksumURL == nil {
				fmt.Printf("No checksum found\n")
			} else {
				resp, err := http.Get(tfVersion.ChecksumURL.String())

				if err != nil {
					log.Fatal(err)
				}

				defer func() {
					if err := resp.Body.Close(); err != nil {
						fmt.Println("Error closing response body")
					}
				}()

				for {
					var checksum []byte
					var filename string

					n, err := fmt.Fscanf(resp.Body, "%64x  %s", &checksum, &filename)

					if err == io.EOF {
						fmt.Printf("No checksum found\n")

						break
					}

					if err != nil {
						log.Fatal(err)
					}

					if n == 2 {
						if filename == path.Base(tfVersion.URL.Path) {
							if !bytes.Equal(h.Sum(nil), checksum) {
								fmt.Printf("Checksum verification failed\n")

								return
							}

							break
						}
					} else {
						fmt.Printf("Bad format\n")
					}
				}

			}

			archive, err := zip.OpenReader(archivePath)

			if err != nil {
				log.Fatal(err)
			}

			defer func() {
				if err := archive.Close(); err != nil {
					fmt.Println("Error closing archive")
				}
			}()

			for _, file := range archive.File {
				if file.FileHeader.Name == "terraform" {
					src, err := file.Open()

					if err != nil {
						log.Fatal(err)
					}

					defer func() {
						if err := src.Close(); err != nil {
							fmt.Println("Error closing source file")
						}
					}()

					dst, err := os.Create(path.Join(tfVersionDirPath, path.Base(file.FileHeader.Name)))

					if err != nil {
						log.Fatal(err)
					}

					defer func() {
						if err := dst.Close(); err != nil {
							fmt.Println("Error closing destination file")
						}
					}()

					_, err = io.Copy(dst, src)

					if err != nil {
						log.Fatal(err)
					}

					err = dst.Chmod(0755)

					if err != nil {
						log.Fatal(err)
					}
				}
			}

			fmt.Printf("Successfully installed Terraform version %s\n", tfVersion.Version)

			return
		}
	}

	fmt.Printf("None of the available Terraform versions matched the constraints\n")
}

func exec(args []string) {
	tfVersionsDir, err := os.Open(tfVersionsDirPath)

	if err != nil {
		log.Fatal(err)
	}

	defer func() {
		if err := tfVersionsDir.Close(); err != nil {
			fmt.Println("Error closing Terraform versions directory")
		}
	}()

	tfVersionDirPaths, err := tfVersionsDir.Readdir(-1)

	if err != nil {
		log.Fatal(err)
	}

	tfVersions := make([]tfVersion, len(tfVersionDirPaths))

	for i, tfVersionDirPath := range tfVersionDirPaths {
		version, err := version.NewVersion(tfVersionDirPath.Name())

		if err != nil {
			log.Fatal(err)
		}

		tfVersions[i] = tfVersion{
			Version: version,
		}
	}

	sortDsc(tfVersions)

	constraints := getConstraints()

	for _, tfVersion := range tfVersions {
		if constraints.Check(tfVersion.Version) {
			tfVersionBinPath := path.Join(tfVersionsDirPath, tfVersion.Version.String(), "terraform")

			if _, err := os.Stat(tfVersionBinPath); os.IsNotExist(err) {
				fmt.Printf("Found Terraform version %s but Terraform binary is missing\n", tfVersion.Version)
				break
			}

			args := append([]string{"terraform"}, args...)
			env := os.Environ()

			err = syscall.Exec(tfVersionBinPath, args, env)

			if err != nil {
				log.Fatal(err)
			}
		}
	}

	fmt.Printf("None of the installed Terraform versions matched the constraints\n")
}
