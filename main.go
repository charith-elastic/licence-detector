package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/build"
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

	"github.com/karrick/godirwalk"
)

var (
	inFlag              = flag.String("in", "-", "Dependency list (output from go list -m -json all)")
	includeIndirectFlag = flag.Bool("includeIndirect", false, "Include indirect dependencies")
	outFlag             = flag.String("out", "-", "Path to output the notice information")
	templateFlag        = flag.String("template", "NOTICE.txt.tmpl", "Path to the template file")

	errLicenceNotFound = errors.New("failed to detect licence")
	goModCache         = filepath.Join(build.Default.GOPATH, "pkg", "mod")
	licenceRegex       = buildLicenceRegex()
)

type Dependencies struct {
	Direct   []LicenceInfo
	Indirect []LicenceInfo
}

type LicenceInfo struct {
	Module
	LicenceFile string
	Error       error
}

type Module struct {
	Path     string     // module path
	Version  string     // module version
	Main     bool       // is this the main module?
	Time     *time.Time // time version was created
	Indirect bool       // is this module only an indirect dependency of main module?
	Dir      string     // directory holding files for this module, if any
}

func buildLicenceRegex() *regexp.Regexp {
	// inspired by https://github.com/src-d/go-license-detector/blob/7961dd6009019bc12778175ef7f074ede24bd128/licensedb/internal/investigation.go#L29
	licenceFileNames := []string{
		`li[cs]en[cs]es?`,
		`legal`,
		`copy(left|right|ing)`,
		`unlicense`,
		`l?gpl([-_ v]?)(\d\.?\d)?`,
		`bsd`,
		`mit`,
		`apache`,
	}

	regexStr := fmt.Sprintf(`^(?i:(%s)(\.(txt|md|rst))?)$`, strings.Join(licenceFileNames, "|"))
	return regexp.MustCompile(regexStr)
}

func main() {
	flag.Parse()
	depInput, err := mkReader(*inFlag)
	if err != nil {
		log.Fatalf("Failed to create reader for %s: %v", *inFlag, err)
	}
	defer depInput.Close()

	dependencies, err := parseDependencies(depInput, *includeIndirectFlag)
	if err != nil {
		log.Fatalf("Failed to parse dependencies: %v", err)
	}

	detectLicences(&dependencies)
	if err := renderNotice(dependencies, *templateFlag, *outFlag); err != nil {
		log.Fatalf("Failed to render notice: %v", err)
	}
}

func mkReader(path string) (io.ReadCloser, error) {
	if path == "-" {
		return ioutil.NopCloser(os.Stdin), nil
	}

	return os.Open(path)
}

func parseDependencies(data io.Reader, includeIndirect bool) (Dependencies, error) {
	deps := Dependencies{}
	decoder := json.NewDecoder(data)
	for {
		var mod Module
		if err := decoder.Decode(&mod); err != nil {
			if err == io.EOF {
				return deps, nil
			}
			return deps, fmt.Errorf("failed to parse dependencies: %w", err)
		}

		if !mod.Main && mod.Dir != "" {
			if mod.Indirect {
				if includeIndirect {
					deps.Indirect = append(deps.Indirect, LicenceInfo{Module: mod})
				}
				continue
			}
			deps.Direct = append(deps.Direct, LicenceInfo{Module: mod})
		}
	}

	sort.Slice(deps.Direct, func(i, j int) bool {
		return deps.Direct[i].Path < deps.Direct[j].Path
	})

	sort.Slice(deps.Indirect, func(i, j int) bool {
		return deps.Indirect[i].Path < deps.Indirect[j].Path
	})

	return deps, nil
}

func detectLicences(deps *Dependencies) {
	for _, depList := range [][]LicenceInfo{deps.Direct, deps.Indirect} {
		for i, dep := range depList {
			depList[i].LicenceFile, depList[i].Error = findLicenceFile(dep.Dir)
			if depList[i].Error != nil && depList[i].Error != errLicenceNotFound {
				panic(fmt.Errorf("unexpected error while processing %s: %v", dep.Path, depList[i].Error))
			}
		}
	}
}

func findLicenceFile(root string) (string, error) {
	errStopWalk := errors.New("stop walk")
	var licenceFile string
	err := godirwalk.Walk(root, &godirwalk.Options{
		Callback: func(osPathName string, dirent *godirwalk.Dirent) error {
			if licenceRegex.MatchString(dirent.Name()) {
				if dirent.IsDir() {
					return filepath.SkipDir
				}
				licenceFile = osPathName
				return errStopWalk
			}
			return nil
		},
		Unsorted: true,
	})

	if err != nil {
		if errors.Is(err, errStopWalk) {
			return licenceFile, nil
		}
		return "", err
	}

	return "", errLicenceNotFound
}

func renderNotice(dependencies Dependencies, templatePath, outputPath string) error {
	funcMap := template.FuncMap{
		"currentYear": CurrentYear,
		"line":        Line,
		"licenceText": LicenceText,
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

	if err := tmpl.Execute(w, dependencies); err != nil {
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

func Line(ch string) string {
	return strings.Repeat(ch, 80)
}

func LicenceText(licInfo LicenceInfo) string {
	if licInfo.Error != nil {
		return licInfo.Error.Error()
	}

	var buf bytes.Buffer
	buf.WriteString("Contents of probable licence file ")
	buf.WriteString(strings.Replace(licInfo.LicenceFile, goModCache, "$GOMODCACHE", -1))
	buf.WriteString(":\n\n")

	f, err := os.Open(licInfo.LicenceFile)
	if err != nil {
		panic(fmt.Errorf("failed to open licence file %s: %v", licInfo.LicenceFile, err))
	}
	defer f.Close()

	_, err = io.Copy(&buf, f)
	if err != nil {
		panic(fmt.Errorf("failed to read licence file %s: %v", licInfo.LicenceFile, err))
	}

	return buf.String()
}
