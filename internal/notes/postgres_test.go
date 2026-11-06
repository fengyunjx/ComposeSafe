package notes

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// A trimmed copy of the structure of a postgresql.org release page.
const pgPage = `<html><head><title>PostgreSQL: Release Notes</title><script>var x = "<b>no</b>";</script></head><body>
<div id="docContent">
<div class="navheader"><table><tr><th>E.13.&nbsp;Release %[1]s</th></tr><tr><td><a href="x">Prev</a></td></tr></table></div>
<div class="sect1" id="RELEASE-%[1]s">
  <div class="titlepage"><h2 class="title" style="clear: both">E.13.&nbsp;Release %[1]s <a href="#R" class="id_link">#</a></h2></div>
  <div class="toc"><dl><dt><a href="#M">E.13.1. Migration to Version %[1]s</a></dt></dl></div>
  <p><strong>Release date:&nbsp;</strong>2024-05-09</p>
  <p>This release contains fixes &amp; improvements.</p>
  <ul><li><p>Fix <code>pg_dump</code> &lt;bug&gt;.</p></li><li><p>Second item</p></li></ul>
</div>
<div class="navfooter"><hr/>Footer text that must not appear</div>
</div></body></html>`

func TestExtractPGNotes(t *testing.T) {
	title, body := ExtractPGNotes(fmt.Sprintf(pgPage, "16.3"))
	if title != "E.13. Release 16.3" {
		t.Errorf("title = %q", title)
	}
	for _, want := range []string{"Release date: 2024-05-09", "fixes & improvements", "- Fix pg_dump <bug>.", "- Second item"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	for _, bad := range []string{"Footer text", "Prev", "var x", "<p>", "&amp;"} {
		if strings.Contains(body, bad) {
			t.Errorf("body must not contain %q", bad)
		}
	}
	if _, b := ExtractPGNotes("<html>nothing</html>"); b != "" {
		t.Error("unrecognised layout must give an empty body")
	}
}

func pgServer(t *testing.T, missing ...string) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ver := strings.Trim(strings.TrimPrefix(r.URL.Path, "/docs/release/"), "/")
		mu.Lock()
		asked = append(asked, ver)
		mu.Unlock()
		for _, m := range missing {
			if ver == m {
				http.NotFound(w, r)
				return
			}
		}
		fmt.Fprintf(w, pgPage, ver)
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

func TestPostgresFetchSameMajor(t *testing.T) {
	srv, asked := pgServer(t)
	p := Postgres{BaseURL: srv.URL + "/docs/release/"}
	got, warns := p.Fetch(t.Context(), ver("16.2"), ver("16.5"))
	if len(warns) != 0 || strings.Join(*asked, ",") != "16.3,16.4,16.5" || len(got) != 3 {
		t.Fatalf("asked=%v warns=%v got=%d", *asked, warns, len(got))
	}
	if got[0].URL != srv.URL+"/docs/release/16.3/" || got[0].Tag != "16.3" || !strings.Contains(got[0].Body, "fixes") {
		t.Errorf("note: %+v", got[0])
	}
}

func TestPostgresFetchCapsMinors(t *testing.T) {
	srv, asked := pgServer(t)
	_, _ = Postgres{BaseURL: srv.URL}.Fetch(t.Context(), ver("16.0"), ver("16.15"))
	if len(*asked) != pgMaxMinors || (*asked)[0] != "16.6" || (*asked)[len(*asked)-1] != "16.15" {
		t.Errorf("asked %v", *asked)
	}
}

func TestPostgresFetchAcrossMajors(t *testing.T) {
	srv, asked := pgServer(t)
	_, _ = Postgres{BaseURL: srv.URL}.Fetch(t.Context(), ver("14.5"), ver("16.4"))
	if strings.Join(*asked, ",") != "15.0,16.0,16.4" {
		t.Errorf("asked %v", *asked)
	}
	// floating major tag: the minor is unknown, so only X.0
	*asked = nil
	_, _ = Postgres{BaseURL: srv.URL}.Fetch(t.Context(), ver("14.5"), ver("16"))
	if strings.Join(*asked, ",") != "15.0,16.0" {
		t.Errorf("floating target asked %v", *asked)
	}
}

func TestPostgresMissingPageBecomesWarning(t *testing.T) {
	srv, _ := pgServer(t, "16.0")
	got, warns := Postgres{BaseURL: srv.URL}.Fetch(t.Context(), ver("15.1"), ver("16.1"))
	if len(got) != 1 || got[0].Tag != "16.1" || len(warns) != 1 || !strings.Contains(warns[0], "16.0") {
		t.Fatalf("got=%v warns=%v", got, warns)
	}
}

func TestPostgresOldVersionsUnsupported(t *testing.T) {
	got, warns := Postgres{BaseURL: "http://127.0.0.1:1"}.Fetch(t.Context(), ver("9.6"), ver("10.1"))
	if len(got) != 0 || len(warns) != 1 {
		t.Errorf("got=%v warns=%v", got, warns)
	}
}

func TestHTMLText(t *testing.T) {
	in := `<div><h3>Title</h3><p>One  <b>two</b></p><ul><li>a</li><li>b &amp; c</li></ul></div>`
	want := "Title\nOne two\n- a\n- b & c"
	if got := HTMLText(in); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}
