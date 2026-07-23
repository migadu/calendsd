package corpus

import (
	"encoding/xml"
	"fmt"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
)

// unreplayable lists tests whose behavior depends on event listeners the PHP
// test registers programmatically — state the recorder cannot capture. They
// are excluded from the corpus permanently.
var unreplayable = map[string]string{
	`Sabre\CalDAV\ExpandEventsFloatingTimeTest::testExpandExport`:           "uses the ICSExportPlugin (?export), out of MVP scope",
	`Sabre\DAV\HTTPPreferParsingTest::testproppatchMinimal`:                 "registers custom propPatch listener",
	`Sabre\DAV\HttpCopyTest::testCopyFileToExistinBlockedCreateDestination`: "registers beforeBind interceptor",
	`Sabre\DAV\HttpMoveTest::testMoveToExistingBlockedDeleteSource`:         "registers beforeUnbind interceptor",
	`Sabre\DAV\HttpPutTest::testPutIntercept`:                               "registers beforeMethod interceptor",
}

// passing lists corpus tests (by originating PHP test) the Go server must
// replay correctly; CI fails if one regresses. Porting a feature adds its
// tests here — the port's progress metric. Tests not listed are reported as
// pending; newly green candidates show up in the test log.
var passing = map[string]bool{
	`Sabre\CalDAV\ExpandEventsDTSTARTandDTENDTest::testExpand`:                         true,
	`Sabre\CalDAV\ExpandEventsDTSTARTandDTENDbyDayTest::testExpandRecurringByDayEvent`: true,
	`Sabre\CalDAV\ExpandEventsDoubleEventsTest::testExpand`:                            true,
	`Sabre\CalDAV\ExpandEventsFloatingTimeTest::testExpandCalendarQuery`:               true,
	`Sabre\CalDAV\ExpandEventsFloatingTimeTest::testExpandMultiGet`:                    true,
	`Sabre\CalDAV\GetEventsByTimerangeTest::testQueryTimerange`:                        true,
	`Sabre\CalDAV\Issue1000Test::testIssue1000`:                                        true,
	`Sabre\CalDAV\Issue203Test::testIssue203`:                                          true,
	`Sabre\CalDAV\Issue205Test::testIssue205`:                                          true,
	`Sabre\CalDAV\Issue211Test::testIssue211`:                                          true,
	`Sabre\CalDAV\Issue220Test::testIssue220`:                                          true,
	`Sabre\CalDAV\Issue228Test::testIssue228`:                                          true,
	`Sabre\DAV\HttpCopyTest::testCopyColl`:                                             true,
	`Sabre\DAV\HttpCopyTest::testCopyCollIntoSubtree`:                                  true,
	`Sabre\DAV\HttpCopyTest::testCopyCollToExisting`:                                   true,
	`Sabre\DAV\HttpCopyTest::testCopyCollToExistingOverwriteF`:                         true,
	`Sabre\DAV\HttpCopyTest::testCopyCollToExistingOverwriteT`:                         true,
	`Sabre\DAV\HttpCopyTest::testCopyCollToSelf`:                                       true,
	`Sabre\DAV\HttpCopyTest::testCopyFile`:                                             true,
	`Sabre\DAV\HttpCopyTest::testCopyFileNonExistantParent`:                            true,
	`Sabre\DAV\HttpCopyTest::testCopyFileToExisting`:                                   true,
	`Sabre\DAV\HttpCopyTest::testCopyFileToExistingOverwriteBadValue`:                  true,
	`Sabre\DAV\HttpCopyTest::testCopyFileToExistingOverwriteF`:                         true,
	`Sabre\DAV\HttpCopyTest::testCopyFileToExistingOverwriteT`:                         true,
	`Sabre\DAV\HttpCopyTest::testCopyFileToSelf`:                                       true,
	`Sabre\DAV\HttpCopyTest::testShallowCopyColl`:                                      true,
	`Sabre\DAV\HttpDeleteTest::testDelete`:                                             true,
	`Sabre\DAV\HttpDeleteTest::testDeleteDirectory`:                                    true,
	`Sabre\DAV\HttpDeleteTest::testDeleteNotFound`:                                     true,
	`Sabre\DAV\HttpDeleteTest::testDeletePreconditions`:                                true,
	`Sabre\DAV\HttpDeleteTest::testDeletePreconditionsFailed`:                          true,
	`Sabre\DAV\HttpGetTest::testGet`:                                                   true,
	`Sabre\DAV\HttpGetTest::testGet404`:                                                true,
	`Sabre\DAV\HttpGetTest::testGet404AsWell`:                                          true,
	`Sabre\DAV\HttpGetTest::testGetCollection`:                                         true,
	`Sabre\DAV\HttpGetTest::testGetDoubleSlash`:                                        true,
	`Sabre\DAV\HttpGetTest::testGetHttp10`:                                             true,
	`Sabre\DAV\HttpGetTest::testGetStreaming`:                                          true,
	`Sabre\DAV\HttpHeadTest::testDoubleAuth`:                                           true,
	`Sabre\DAV\HttpHeadTest::testHEAD`:                                                 true,
	`Sabre\DAV\HttpHeadTest::testHEADCollection`:                                       true,
	`Sabre\DAV\HttpMoveTest::testMove`:                                                 true,
	`Sabre\DAV\HttpMoveTest::testMoveToExisting`:                                       true,
	`Sabre\DAV\HttpMoveTest::testMoveToExistingOverwriteF`:                             true,
	`Sabre\DAV\HttpMoveTest::testMoveToExistingOverwriteT`:                             true,
	`Sabre\DAV\HttpMoveTest::testMoveToSelf`:                                           true,
	`Sabre\DAV\HTTPPreferParsingTest::testpropfindMinimal`:                             true,
	`Sabre\DAV\HTTPPreferParsingTest::testproppatchMinimalError`:                       true,
	`Sabre\DAV\HttpPutTest::testFinderPutFail`:                                         true,
	`Sabre\DAV\HttpPutTest::testFinderPutSuccess`:                                      true,
	`Sabre\DAV\HttpPutTest::testPut`:                                                   true,
	`Sabre\DAV\HttpPutTest::testPutContentRange`:                                       true,
	`Sabre\DAV\HttpPutTest::testPutExisting`:                                           true,
	`Sabre\DAV\HttpPutTest::testPutExistingIfMatchCorrect`:                             true,
	`Sabre\DAV\HttpPutTest::testPutExistingIfMatchStar`:                                true,
	`Sabre\DAV\HttpPutTest::testPutExistingIfNoneMatchStar`:                            true,
	`Sabre\DAV\HttpPutTest::testPutIfMatchStar`:                                        true,
	`Sabre\DAV\HttpPutTest::testPutIfNoneMatchStar`:                                    true,
	`Sabre\DAV\HttpPutTest::testPutParentCollectionDoesNotExist`:                       true,
	`Sabre\DAV\HttpPutTest::testPutParentIsNotCollection`:                              true,
	`Sabre\DAV\PropertyStorage\PluginTest::testMove`:                                   true,
	`Sabre\DAVACL\AclPrincipalPropSetReportTest::testReport`:                           true,
	`Sabre\DAVACL\AclPrincipalPropSetReportTest::testReportDepth1`:                     true,
	`Sabre\DAVACL\PrincipalMatchTest::testPrincipalMatch`:                              true,
	`Sabre\DAVACL\PrincipalMatchTest::testPrincipalMatchPrincipalProperty`:             true,
	`Sabre\DAVACL\PrincipalMatchTest::testPrincipalMatchProp`:                          true,
}

// outOfScopePrefixes are sabre plugins outside the MVP port scope
// (PLAN.md §1): PATCH partial updates, sharing, scheduling, subscriptions,
// and the export/jCal conveniences.
var outOfScopePrefixes = []string{
	`Sabre\DAV\PartialUpdate\`,
	`Sabre\DAV\Sharing\`,
	`Sabre\CalDAV\Schedule\`,
	`Sabre\CalDAV\SharingPluginTest`,
	`Sabre\CalDAV\Subscriptions\`,
	`Sabre\CalDAV\ICSExportPluginTest`,
	`Sabre\CalDAV\JCalTransformTest`,
}

// TestCorpus replays the extracted sabre corpus. Records are grouped by
// originating test and replayed in sequence against one seeded server, since
// later requests depend on earlier mutations.
func TestCorpus(t *testing.T) {
	records, err := Load("../../testdata/corpus")
	if err != nil {
		t.Fatalf("loading corpus: %v", err)
	}
	if len(records) == 0 {
		t.Skip("no corpus records extracted; run the CorpusRecorder (see package doc)")
	}

	groups := map[string][]Record{}
	var names []string
	for _, rec := range records {
		if _, seen := groups[rec.Test]; !seen {
			names = append(names, rec.Test)
		}
		groups[rec.Test] = append(groups[rec.Test], rec)
	}
	sort.Strings(names)

	var green, newlyGreen []string
	var pending, outOfScope int
	for _, name := range names {
		recs := groups[name]
		sort.Slice(recs, func(i, j int) bool { return recs[i].Seq < recs[j].Seq })

		if recs[0].UsesUnsupportedFeatures() {
			outOfScope++
			continue
		}
		if _, skip := unreplayable[name]; skip {
			outOfScope++
			continue
		}
		if hasAnyPrefix(name, outOfScopePrefixes) {
			outOfScope++
			continue
		}
		if needsLaterPhase(&recs[0]) {
			pending++
			continue
		}

		mismatches := replayGroup(recs)
		if len(mismatches) == 0 {
			green = append(green, name)
			if !passing[name] {
				newlyGreen = append(newlyGreen, name)
			}
			continue
		}
		pending++
		if passing[name] {
			t.Run(subtestName(name), func(t *testing.T) {
				for _, m := range mismatches {
					t.Error(m)
				}
			})
		} else if os.Getenv("CORPUS_DEBUG") != "" {
			t.Logf("pending %q:\n%s", name, strings.Join(mismatches, "\n"))
		}
	}

	t.Logf("corpus coverage: %d green (%d in passing set), %d pending, %d out of MVP scope (of %d tests)",
		len(green), len(passing), pending+len(green)-len(passing), outOfScope, len(names))
	for _, name := range newlyGreen {
		t.Logf("newly green (add to passing): %q,", name)
	}
	for name := range passing {
		if !contains(green, name) {
			t.Errorf("regression: %q is in the passing set but no longer green", name)
		}
	}
}

// needsLaterPhase filters tests whose fixtures require ports that haven't
// landed yet (property storage).
func needsLaterPhase(r *Record) bool {
	return r.Setup.SetupPropertyStorage
}

// replayGroup replays a test's records in order against one seeded server
// and returns human-readable mismatches (empty = fully green).
func replayGroup(recs []Record) []string {
	srv, err := BuildServer(&recs[0])
	if err != nil {
		return []string{fmt.Sprintf("unseedable: %v", err)}
	}

	var mismatches []string
	for _, rec := range recs {
		mismatches = append(mismatches, replayOne(srv, &rec)...)
	}
	return mismatches
}

func replayOne(srv *dav.Server, rec *Record) []string {
	req := httptest.NewRequest(rec.Request.Method, orSlash(rec.Request.URL), strings.NewReader(rec.Request.Body))
	for name, vals := range rec.Request.Headers {
		for _, v := range vals {
			req.Header.Add(name, v)
		}
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	label := fmt.Sprintf("seq %d %s %s", rec.Seq, rec.Request.Method, rec.Request.URL)
	var out []string
	if w.Code != rec.Response.Status {
		out = append(out, fmt.Sprintf("%s: status = %d, sabre = %d\nours: %s",
			label, w.Code, rec.Response.Status, w.Body.String()))
	}
	if diff := diffBodies(w.Body.String(), rec.Response.Body); diff != "" {
		out = append(out, label+": "+diff)
	}
	if want := rec.Response.Headers["ETag"]; len(want) > 0 {
		if got := w.Header().Get("ETag"); got != want[0] {
			out = append(out, fmt.Sprintf("%s: ETag = %q, sabre = %q", label, got, want[0]))
		}
	}
	return out
}

func orSlash(u string) string {
	if u == "" {
		return "/"
	}
	return u
}

// diffBodies compares semantically: {DAV:}error bodies by exception class
// (sabre's debug fields are masked), other XML via the canonicalizer,
// anything else byte-exact. Empty result means equal.
func diffBodies(got, want string) string {
	if got == want {
		return ""
	}
	gotExc, gotIsErr := errorException(got)
	wantExc, wantIsErr := errorException(want)
	if gotIsErr || wantIsErr {
		if gotExc != wantExc {
			return fmt.Sprintf("exception = %q, sabre = %q", gotExc, wantExc)
		}
		return ""
	}
	if looksLikeXML(got) && looksLikeXML(want) {
		cGot, errG := davxml.CanonicalizeFunc(strings.NewReader(got), canonicalizeICS)
		cWant, errW := davxml.CanonicalizeFunc(strings.NewReader(want), canonicalizeICS)
		if errG == nil && errW == nil {
			if cGot != cWant {
				return fmt.Sprintf("XML bodies differ:\n--- ours ---\n%s\n--- sabre ---\n%s", cGot, cWant)
			}
			return ""
		}
	}
	return fmt.Sprintf("bodies differ:\n--- ours ---\n%q\n--- sabre ---\n%q", got, want)
}

// errorException extracts the sabre exception class from a {DAV:}error body.
func errorException(body string) (string, bool) {
	type davError struct {
		XMLName   xml.Name `xml:"DAV: error"`
		Exception string   `xml:"http://sabredav.org/ns exception"`
	}
	var e davError
	if err := xml.Unmarshal([]byte(body), &e); err != nil {
		return "", false
	}
	return e.Exception, true
}

func looksLikeXML(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "<?xml") || strings.HasPrefix(s, "<")
}

func subtestName(test string) string {
	return strings.NewReplacer("\\", "_", "::", "/").Replace(test)
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
