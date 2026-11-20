package core

import (
	"encoding/json"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// replayBatch is invented data modelled on the sweep's first run on 2026-11-09
// (WP-795), not copied from production: each class's instances as a class
// task's text would give them, and the flake tasks behind two flaky-check
// classes with their logs cut to the failing lines. The projects (Harbor,
// Dockside, Quarry, Orchard, Juno), their files and their findings are made
// up to keep the shapes the real run had: findings copied by an audit of the
// same pull request, a class whose instances all sit in two units, a bug title
// that is the hub of two audits, CI flakes that repeat before their task
// closes. WP's own items are kept as they were. A pull request's unit is its
// number here, its id in production. An instance's own time isn't in a task,
// so each carries its class's.
type replayBatch struct {
	Items []struct {
		Project string `json:"project"`
		Class   string `json:"cls"`
		Source  string `json:"source"`
		ID      string `json:"id"`
		Unit    string `json:"unit"`
		At      int64  `json:"at"`
		File    string `json:"file"`
		Title   string `json:"title"`
		Problem string `json:"problem"`
	} `json:"items"`
	Flakes []struct {
		Project     string `json:"project"`
		Key         string `json:"key"`
		Title       string `json:"title"`
		At          int64  `json:"at"`
		Closed      int64  `json:"closed"`
		Description string `json:"description"`
		Race        bool   `json:"race"`
	} `json:"flakes"`
}

// replayFirstBatch runs the batch through what the sweep does with its
// evidence, and answers with what each project would now file and which class
// each instance came from.
func replayFirstBatch(t *testing.T) (map[string][]*mistakeGroup, map[string]string) {
	t.Helper()
	raw, err := os.ReadFile("testdata/mistake_classes_replay.json")
	if err != nil {
		t.Fatal(err)
	}
	var batch replayBatch
	if err := json.Unmarshal(raw, &batch); err != nil {
		t.Fatal(err)
	}
	byProject, classOf := map[string][]*mistake{}, map[string]string{}
	for _, it := range batch.Items {
		m := &mistake{source: it.Source, id: it.ID, unit: it.Unit, at: it.At, keys: []string{it.File}, name: mistakeSentence(it.Title), text: it.Title}
		m.words = mistakeWords(it.Title)
		if it.Source == "audit" {
			m.words = mistakeWords(it.Title + ". " + it.Problem)
		}
		byProject[it.Project] = append(byProject[it.Project], m)
		classOf[it.Project+"/"+it.ID] = it.Class
	}
	flakes := map[string][]*mistake{}
	for i, f := range batch.Flakes {
		m := flakeMistake(int64(i+1), f.At, f.Closed, f.Key, f.Title, f.Description, f.Race)
		flakes[f.Project] = append(flakes[f.Project], m)
		classOf[f.Project+"/"+m.id] = map[string]string{"JUNO": "JUNO-62", "QRY": "QRY-32"}[f.Project]
	}
	plans := map[string][]*mistakeGroup{}
	for _, project := range slices.Sorted(maps.Keys(byProject)) {
		joinOpenFlakes(flakes[project])
		plans[project] = planMistakes(dropFindingCopies(append(byProject[project], flakes[project]...)), nil).fresh
	}
	return plans, classOf
}

func TestFirstBatchReplayed(t *testing.T) {
	plans, classOf := replayFirstBatch(t)
	titles := map[string]string{}
	for project, fresh := range plans {
		for _, g := range fresh {
			classes := map[string]bool{}
			for _, m := range g.members {
				classes[classOf[project+"/"+m.id]] = true
			}
			if len(classes) != 1 {
				t.Errorf("%s mixes classes %v", g.key, classes)
			}
			for class := range classes {
				titles[class] = mistakeTitle(mistakeLabel(g))
			}
		}
	}
	for _, gone := range []string{"HARB-41", "DOCK-71", "JUNO-62", "QRY-32"} {
		if title, filed := titles[gone]; filed {
			t.Errorf("%s still files: %q", gone, title)
		}
	}
	for class, title := range titles {
		label := strings.TrimPrefix(title, "Recurring mistake: ")
		if longForTitle(title) || strings.Contains(label, ".go (") || strings.Contains(label, ".py (") || len(strings.Fields(label)) < 4 {
			t.Errorf("%s: %q doesn't say what went wrong", class, title)
		}
	}
	want := map[string]string{
		"DOCK-72": "Recurring mistake: A wrapped berth-draught question is not required in an auto-sent reply",
		"HARB-42": "Recurring mistake: The pump's final skip report can hold Close for the full tail drain",
		"JUNO-61": "Recurring mistake: Cloned albums: limit Data to who could see the source at clone time",
		"ORCH-22": "Recurring mistake: No price sanity checks: boxes of pears at €0.01/kg, Fieldpost outliers up to 17x",
		"QRY-31":  "Recurring mistake: Lane counts past its reported capacity (batch-b 27 of 20)",
		"WP-792":  "Recurring mistake: Review-held ship of an existing pull request never claims it",
		"WP-793":  "Recurring mistake: wp next said 'You already hold this task' for a task another live session holds",
	}
	if !maps.Equal(titles, want) {
		got, _ := json.MarshalIndent(titles, "", " ")
		t.Errorf("titles:\n%s", got)
	}
}
