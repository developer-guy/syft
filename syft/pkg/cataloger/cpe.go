package cataloger

import (
	"bufio"
	"bytes"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/anchore/syft/internal"
	"github.com/anchore/syft/syft/pkg"
	"github.com/facebookincubator/nvdtools/wfn"
)

var productCandidatesByPkgType = candidateStore{
	pkg.JavaPkg: {
		"springframework": []string{"spring_framework", "springsource_spring_framework"},
		"spring-core":     []string{"spring_framework", "springsource_spring_framework"},
	},
	pkg.NpmPkg: {
		"hapi":             []string{"hapi_server_framework"},
		"handlebars.js":    []string{"handlebars"},
		"is-my-json-valid": []string{"is_my_json_valid"},
		"mustache":         []string{"mustache.js"},
	},
	pkg.GemPkg: {
		"Arabic-Prawn":        []string{"arabic_prawn"},
		"bio-basespace-sdk":   []string{"basespace_ruby_sdk"},
		"cremefraiche":        []string{"creme_fraiche"},
		"html-sanitizer":      []string{"html_sanitizer"},
		"sentry-raven":        []string{"raven-ruby"},
		"RedCloth":            []string{"redcloth_library"},
		"VladTheEnterprising": []string{"vladtheenterprising"},
		"yajl-ruby":           []string{"yajl-ruby_gem"},
	},
	pkg.PythonPkg: {
		"python-rrdtool": []string{"rrdtool"},
	},
}

var cpeFilters = []filterFn{
	func(cpe pkg.CPE, p pkg.Package) bool {
		// jira / atlassian should not apply to clients
		if cpe.Product == "jira" && strings.Contains(strings.ToLower(p.Name), "client") {
			if cpe.Vendor == wfn.Any || cpe.Vendor == "jira" || cpe.Vendor == "atlassian" {
				return true
			}
		}
		return false
	},
	// nolint: goconst
	func(cpe pkg.CPE, p pkg.Package) bool {
		// jenkins server should only match against a product with the name jenkins
		if cpe.Product == "jenkins" && !strings.Contains(strings.ToLower(p.Name), "jenkins") {
			if cpe.Vendor == wfn.Any || cpe.Vendor == "jenkins" || cpe.Vendor == "cloudbees" {
				return true
			}
		}
		return false
	},
}

type filterFn func(cpe pkg.CPE, p pkg.Package) bool

// this is a static mapping of known package names (keys) to official cpe names for each package
type candidateStore map[pkg.Type]map[string][]string

func (s candidateStore) getCandidates(t pkg.Type, key string) []string {
	if _, ok := s[t]; !ok {
		return nil
	}
	value, ok := s[t][key]
	if !ok {
		return nil
	}

	return value
}

func newCPE(product, vendor, version, targetSW string) wfn.Attributes {
	cpe := *(wfn.NewAttributesWithAny())
	cpe.Part = "a"
	cpe.Product = product
	cpe.Vendor = vendor
	cpe.Version = version
	cpe.TargetSW = targetSW

	return cpe
}

func filterCPEs(cpes []pkg.CPE, p pkg.Package, filters ...filterFn) (result []pkg.CPE) {
cpeLoop:
	for _, cpe := range cpes {
		for _, fn := range filters {
			if fn(cpe, p) {
				continue cpeLoop
			}
		}
		// all filter functions passed on filtering this CPE
		result = append(result, cpe)
	}
	return result
}

// generatePackageCPEs Create a list of CPEs, trying to guess the vendor, product tuple and setting TargetSoftware if possible
func generatePackageCPEs(p pkg.Package) []pkg.CPE {
	targetSws := candidateTargetSoftwareAttrs(p)
	vendors := candidateVendors(p)
	products := candidateProducts(p)

	if len(products) == 0 {
		return nil
	}

	keys := internal.NewStringSet()
	cpes := make([]pkg.CPE, 0)
	for _, product := range products {
		for _, vendor := range vendors {
			for _, targetSw := range append([]string{wfn.Any}, targetSws...) {
				// prevent duplicate entries...
				key := fmt.Sprintf("%s|%s|%s|%s", product, vendor, p.Version, targetSw)
				if keys.Contains(key) {
					continue
				}
				keys.Add(key)

				// add a new entry...
				c := newCPE(product, vendor, p.Version, targetSw)
				cpes = append(cpes, c)
			}
		}
	}

	// filter out any known combinations that don't accurately represent this package
	cpes = filterCPEs(cpes, p, cpeFilters...)

	sort.Sort(ByCPESpecificity(cpes))

	return cpes
}

func candidateTargetSoftwareAttrs(p pkg.Package) []string {
	// TODO: would be great to allow these to be overridden by user data/config
	var targetSw []string
	switch p.Language {
	case pkg.Java:
		targetSw = append(targetSw, candidateTargetSoftwareAttrsForJava(p)...)
	case pkg.JavaScript:
		targetSw = append(targetSw, "node.js", "nodejs")
	case pkg.Ruby:
		targetSw = append(targetSw, "ruby", "rails")
	case pkg.Python:
		targetSw = append(targetSw, "python")
	case pkg.Go:
		targetSw = append(targetSw, "go", "golang")
	}

	return targetSw
}

func candidateTargetSoftwareAttrsForJava(p pkg.Package) []string {
	// Use the more specific indicator if available
	if p.Type == pkg.JenkinsPluginPkg {
		return []string{"jenkins", "cloudbees_jenkins"}
	}

	return []string{"java", "maven"}
}

func candidateVendors(p pkg.Package) []string {
	// TODO: Confirm whether using products as vendors is helpful to the matching process
	vendors := candidateProducts(p)

	switch p.Language {
	case pkg.Java:
		if p.MetadataType == pkg.JavaMetadataType {
			vendors = append(vendors, candidateVendorsForJava(p)...)
		}
	case pkg.Go:
		// replace all candidates with only the golang-specific helper
		vendors = nil
		vendor := candidateVendorForGo(p.Name)
		if vendor != "" {
			vendors = []string{vendor}
		}
	}

	// try swapping hyphens for underscores, vice versa, and removing separators altogether
	vendors = normalizeAllSeparators(vendors)

	// generate sub-selections of each candidate based on separators (e.g. jenkins-ci -> [jenkins, jenkins-ci])
	vendors = generateAllSubSelections(vendors)

	return removeDuplicateValues(vendors)
}

func candidateProducts(p pkg.Package) []string {
	products := []string{p.Name}

	switch {
	case p.Language == pkg.Python:
		if !strings.HasPrefix(p.Name, "python") {
			products = append(products, "python-"+p.Name)
		}
	case p.MetadataType == pkg.JavaMetadataType:
		products = append(products, candidateProductsForJava(p)...)
	case p.Language == pkg.Go:
		// replace all candidates with only the golang-specific helper
		products = nil
		prod := candidateProductForGo(p.Name)
		if prod != "" {
			products = []string{prod}
		}
	}

	// try swapping hyphens for underscores, vice versa, and removing separators altogether
	products = normalizeAllSeparators(products)

	// prepend any known product name swaps prepended to the results
	products = append(productCandidatesByPkgType.getCandidates(p.Type, p.Name), products...)

	return removeDuplicateValues(products)
}

// candidateProductForGo attempts to find a single product name in a best-effort attempt. This implementation prefers
// to return no vendor over returning potentially nonsensical results.
func candidateProductForGo(name string) string {
	// note: url.Parse requires a scheme for correct processing, which a golang module will not have, so one is provided.
	u, err := url.Parse("http://" + name)
	if err != nil {
		return ""
	}

	cleanPath := strings.Trim(u.Path, "/")
	pathElements := strings.Split(cleanPath, "/")

	switch u.Host {
	case "golang.org", "gopkg.in":
		return cleanPath
	case "google.golang.org":
		return pathElements[0]
	}

	if len(pathElements) < 2 {
		return ""
	}

	return pathElements[1]
}

// candidateVendorForGo attempts to find a single vendor name in a best-effort attempt. This implementation prefers
// to return no vendor over returning potentially nonsensical results.
func candidateVendorForGo(name string) string {
	// note: url.Parse requires a scheme for correct processing, which a golang module will not have, so one is provided.
	u, err := url.Parse("http://" + name)
	if err != nil {
		return ""
	}

	cleanPath := strings.Trim(u.Path, "/")

	switch u.Host {
	case "google.golang.org":
		return "google"
	case "golang.org":
		return "golang"
	case "gopkg.in":
		return ""
	}

	pathElements := strings.Split(cleanPath, "/")
	if len(pathElements) < 2 {
		return ""
	}
	return pathElements[0]
}

func candidateProductsForJava(p pkg.Package) []string {
	// TODO: we could get group-id-like info from the MANIFEST.MF "Automatic-Module-Name" field
	// for more info see pkg:maven/commons-io/commons-io@2.8.0 within cloudbees/cloudbees-core-mm:2.263.4.2
	// at /usr/share/jenkins/jenkins.war:WEB-INF/plugins/analysis-model-api.hpi:WEB-INF/lib/commons-io-2.8.0.jar
	if product, _ := productAndVendorFromPomPropertiesGroupID(p); product != "" {
		// ignore group ID info from a jenkins plugin, as using this info may imply that this package
		// CPE belongs to the cloudbees org (or similar) which is wrong.
		if p.Type == pkg.JenkinsPluginPkg && strings.ToLower(product) == "jenkins" {
			return nil
		}
		return []string{product}
	}

	return nil
}

func candidateVendorsForJava(p pkg.Package) []string {
	if _, vendor := productAndVendorFromPomPropertiesGroupID(p); vendor != "" {
		return []string{vendor}
	}

	return nil
}

func productAndVendorFromPomPropertiesGroupID(p pkg.Package) (string, string) {
	groupID := groupIDFromPomProperties(p)
	if !shouldConsiderGroupID(groupID) {
		return "", ""
	}

	if !internal.HasAnyOfPrefixes(groupID, "com", "org") {
		return "", ""
	}

	fields := strings.Split(groupID, ".")
	if len(fields) < 3 {
		return "", ""
	}

	product := fields[2]
	vendor := fields[1]
	return product, vendor
}

func groupIDFromPomProperties(p pkg.Package) string {
	metadata, ok := p.Metadata.(pkg.JavaMetadata)
	if !ok {
		return ""
	}

	if metadata.PomProperties == nil {
		return ""
	}

	return metadata.PomProperties.GroupID
}

func shouldConsiderGroupID(groupID string) bool {
	if groupID == "" {
		return false
	}

	excludedGroupIDs := append([]string{pkg.JiraPluginPomPropertiesGroupID}, pkg.JenkinsPluginPomPropertiesGroupIDs...)

	return !internal.HasAnyOfPrefixes(groupID, excludedGroupIDs...)
}

func generateAllSubSelections(fields []string) (results []string) {
	for _, field := range fields {
		results = append(results, generateSubSelections(field)...)
	}
	return results
}

// generateSubSelections attempts to split a field by hyphens and underscores and return a list of sensible sub-selections
// that can be used as product or vendor candidates.
func generateSubSelections(field string) (results []string) {
	scanner := bufio.NewScanner(strings.NewReader(field))
	scanner.Split(scanHyphenOrUnderscore)
	var lastToken uint8
	for scanner.Scan() {
		rawCandidate := scanner.Text()
		if len(rawCandidate) == 0 {
			break
		}

		candidate := strings.TrimFunc(rawCandidate, trimHyphenOrUnderscore)

		// capture the result (if there is content)
		if len(candidate) > 0 {
			if len(results) > 0 {
				results = append(results, results[len(results)-1]+string(lastToken)+candidate)
			} else {
				results = append(results, candidate)
			}
		}

		// keep track of the trailing separator for the next loop
		lastToken = rawCandidate[len(rawCandidate)-1]
	}
	return results
}

func trimHyphenOrUnderscore(r rune) bool {
	switch r {
	case '-', '_':
		return true
	}
	return false
}

// scanHyphenOrUnderscore splits on hyphen or underscore and includes the separator in the split
func scanHyphenOrUnderscore(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "-_"); i >= 0 {
		return i + 1, data[0 : i+1], nil
	}

	if atEOF {
		return len(data), data, nil
	}

	return 0, nil, nil
}

func normalizeAllSeparators(fields []string) []string {
	var results = make([]string, 0, len(fields))
	for _, field := range fields {
		// always include the original value
		results = append(results, field)
		hasHyphen := strings.Contains(field, "-")
		hasUnderscore := strings.Contains(field, "_")

		if hasHyphen {
			// provide variations of hyphen candidates with an underscore and no separator
			results = append(results, strings.ReplaceAll(field, "-", "_"))
			results = append(results, strings.ReplaceAll(field, "-", ""))
		}

		if hasUnderscore {
			// provide variations of underscore candidates with a hyphen and no separator
			results = append(results, strings.ReplaceAll(field, "_", "-"))
			results = append(results, strings.ReplaceAll(field, "_", ""))
		}
	}
	return results
}

func removeDuplicateValues(values []string) (results []string) {
	observed := make(map[string]struct{})
	for _, entry := range values {
		if _, value := observed[entry]; !value {
			observed[entry] = struct{}{}
			results = append(results, entry)
		}
	}
	return results
}
