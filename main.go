package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/mitchellh/go-spdx"
	"gopkg.in/src-d/go-license-detector.v3/licensedb"
	"gopkg.in/src-d/go-license-detector.v3/licensedb/api"
	"gopkg.in/src-d/go-license-detector.v3/licensedb/filer"
)

const (
	apache2 = "Apache-2.0"
	unknown = "unknown"
)

var (
	inFlag       = flag.String("in", "-", "Dependency list (output from go list -deps -json)")
	outFlag      = flag.String("out", "-", "Path to output the notice information")
	templateFlag = flag.String("template", "NOTICE.txt.tmpl", "Path to the template file")

	copyrightRegex = regexp.MustCompile(`[Cc]opyright\s+\(\s*[Cc]\s*\)`)
)

type Package struct {
	ImportPath string `json:"ImportPath"`
	GoRoot     bool   `json:"GoRoot"`
	Standard   bool   `json:"Standard"`
	DepOnly    bool   `json:"DepOnly"`
	Root       string `json:"Root"`
}

type LicenceInfo struct {
	Pkg       string
	Root      string
	Copyright string
}

type NoticeData struct {
	LicenceName  string
	LicenceText  string
	Dependencies []LicenceInfo
}

func main() {
	flag.Parse()
	depInput, err := mkReader(*inFlag)
	if err != nil {
		log.Fatalf("failed to create reader for %s: %v", *inFlag, err)
	}
	defer depInput.Close()

	depList, err := parseDependencies(depInput)
	if err != nil {
		log.Fatalf("Failed to parse dependencies: %v", err)
	}

	licences, err := detectLicences(depList)
	if err != nil {
		log.Fatalf("Failed to detect licences: %v", err)
	}

	noticeData, err := generateNoticeData(licences)
	if err != nil {
		log.Fatalf("Failed to generate notice data: %v", err)
	}

	if err := renderNotice(noticeData, *templateFlag, *outFlag); err != nil {
		log.Fatalf("Failed to render notice: %v", err)
	}
}

func mkReader(path string) (io.ReadCloser, error) {
	if path == "-" {
		return ioutil.NopCloser(os.Stdin), nil
	}

	return os.Open(path)
}

func parseDependencies(data io.Reader) ([]Package, error) {
	decoder := json.NewDecoder(data)
	var packageList []Package
	for {
		var pkg Package
		if err := decoder.Decode(&pkg); err != nil {
			if err == io.EOF {
				return packageList, nil
			}
			return packageList, fmt.Errorf("failed to parse dependencies: %w", err)
		}

		if !pkg.GoRoot && pkg.DepOnly {
			packageList = append(packageList, pkg)
		}
	}
}

func detectLicences(deps []Package) (map[string][]LicenceInfo, error) {
	groupedLicences := make(map[string][]LicenceInfo)
	for _, dep := range deps {
		info := LicenceInfo{Pkg: dep.ImportPath, Root: dep.Root}

		f, err := filer.FromDirectory(dep.Root)
		if err != nil {
			return groupedLicences, fmt.Errorf("failed to create filer for path %s: %w", dep.Root, err)
		}

		licences, err := licensedb.Detect(f)
		if err != nil {
			if err == licensedb.ErrNoLicenseFound {
				groupedLicences[unknown] = append(groupedLicences[unknown], info)
				continue
			}
			return groupedLicences, fmt.Errorf("failed to detect licence for %s: %w", dep.ImportPath, err)
		}

		name, file := determineBestLicence(licences)
		// TODO fail?
		info.Copyright, _ = detectCopyright(name, filepath.Join(dep.Root, file))

		groupedLicences[name] = append(groupedLicences[name], info)
	}

	return groupedLicences, nil
}

func determineBestLicence(licences map[string]api.Match) (name, file string) {
	var maxConfidence float32
	for licName, licInfo := range licences {
		if licInfo.Confidence > maxConfidence {
			maxConfidence = licInfo.Confidence
			name = licName
			for licFile, confidence := range licInfo.Files {
				if confidence == licInfo.Confidence {
					file = licFile
					break
				}
			}
		}
	}

	return
}

func detectCopyright(licence, filePath string) (string, error) {
	if licence == apache2 {
		return "", nil
	}

	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if copyrightRegex.MatchString(line) {
			return line, nil
		}
	}

	return "", scanner.Err()
}

func generateNoticeData(licences map[string][]LicenceInfo) ([]NoticeData, error) {
	var notices []NoticeData
	for licID, licInfo := range licences {
		if licID == unknown {
			notices = append(notices, NoticeData{LicenceName: "Licence Unknown", Dependencies: licInfo})
			continue
		}

		licData, err := spdx.License(licID)
		if err != nil {
			return nil, fmt.Errorf("failed to lookup licence [%s]: %w", licID, err)
		}

		notice := NoticeData{
			LicenceName:  licData.Name,
			LicenceText:  licData.Text,
			Dependencies: licInfo,
		}
		notices = append(notices, notice)
	}

	sort.Slice(notices, func(i, j int) bool {
		return notices[i].LicenceName < notices[j].LicenceName
	})

	return notices, nil
}

func renderNotice(noticeData []NoticeData, templatePath, outputPath string) error {
	funcMap := template.FuncMap{
		"currentYear": CurrentYear,
		"header":      Header,
	}
	tmpl, err := template.New(filepath.Base(templatePath)).Funcs(funcMap).ParseFiles(templatePath)
	if err != nil {
		return fmt.Errorf("failed to parse template at %s: %w", templatePath, err)
	}

	w, cleanup, err := mkWriter(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create output file %s: %w", outputPath, err)
	}
	defer cleanup()

	if err := tmpl.Execute(w, noticeData); err != nil {
		return fmt.Errorf("failed to render template: %w", err)
	}

	return nil
}

func mkWriter(path string) (io.Writer, func(), error) {
	if path == "-" {
		return os.Stdout, func() {}, nil
	}

	f, err := os.Create(path)
	return f, func() { f.Close() }, err
}

func CurrentYear() string {
	return strconv.Itoa(time.Now().Year())
}

func Header(text string) string {
	line := strings.Repeat("-", len(text))
	return strings.Join([]string{line, text, line}, "\n")
}
