package project

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Ejection is the exit. Generated configuration that cannot be inspected or
// left is the thing people refuse to adopt, so Onebox writes the runtime into
// the repository and hands ownership over permanently.
//
// Two properties matter more than convenience here.
//
// The written file must be ordinary Compose. The overlay Onebox adds —
// identity labels, routing labels, the ingress network — is stripped before
// writing, because on the next generation those workloads take the
// Compose-reference path and that path refuses a service already carrying
// them. Without stripping, ejection produces a project that can never generate
// again.
//
// The project file must survive being rewritten. It is a file people maintain
// by hand, with comments explaining why things are the way they are, so the
// rewrite goes through the YAML node tree rather than a decode-and-re-encode
// that would silently discard every one of them.

// EjectResult reports what moved.
type EjectResult struct {
	Runtime   string   `json:"runtime"`
	Workloads []string `json:"workloads"`
}

// Eject writes the generated runtime to dest and repoints the affected
// workloads at it. dest is a repository path relative to the project file.
func (r *Resolved) Eject(dest, releaseID string, images Images, overwrite bool) (*EjectResult, error) {
	if dest == "" {
		dest = "compose.yaml"
	}
	target, err := resolveRepoPath(r.Dir, dest)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(target); err == nil && !overwrite {
		return nil, errf("eject_destination_exists", dest, "",
			"%q already exists; move it or pass the flag that allows overwriting", dest)
	}

	// Only what Onebox generates can be ejected. A workload that already
	// references Compose is the user's file already, and moving it would
	// duplicate a service that lives somewhere else.
	generated := map[string]bool{}
	for _, name := range sortedKeys(r.Workloads) {
		if r.Workloads[name].Compose == "" {
			generated[name] = true
		}
	}
	if len(generated) == 0 {
		return nil, errf("eject_nothing_to_do", "workloads", "",
			"every workload already references a Compose file; there is nothing left to hand over")
	}

	rendered, err := r.Render(r.Env, releaseID, images)
	if err != nil {
		return nil, err
	}
	stripped, names, err := stripOverlay(rendered.Bytes, generated)
	if err != nil {
		return nil, err
	}

	// Write and rename before touching the project. An interruption then leaves
	// the project still pointing at the generator rather than at a file that
	// may not exist.
	tmp := target + ".ob-tmp"
	if err := os.WriteFile(tmp, stripped, 0o600); err != nil {
		return nil, errf("eject_failed", dest, "", "cannot write %q: %v", dest, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return nil, errf("eject_failed", dest, "", "cannot place %q: %v", dest, err)
	}

	if err := repointProject(r.projectFile(), dest, names); err != nil {
		return nil, err
	}
	return &EjectResult{Runtime: dest, Workloads: names}, nil
}

func (r *Resolved) projectFile() string {
	return filepath.Join(r.Dir, "ob.yml")
}

// stripOverlay removes what Onebox adds and reports which workloads were
// generated rather than referenced.
func stripOverlay(runtime []byte, generated map[string]bool) ([]byte, []string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(runtime, &doc); err != nil {
		return nil, nil, errf("eject_failed", "", "", "cannot read the generated runtime: %v", err)
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}

	var names []string
	services := mapValue(root, "services")
	if services == nil {
		return nil, nil, errf("eject_failed", "", "", "the generated runtime declares no services")
	}
	var kept []*yaml.Node
	for i := 0; i+1 < len(services.Content); i += 2 {
		name, svc := services.Content[i].Value, services.Content[i+1]
		if !generated[name] {
			continue
		}
		names = append(names, name)
		dropLabels(svc)
		dropIngress(svc)
		kept = append(kept, services.Content[i], svc)
	}
	services.Content = kept
	// The ingress network is external and belongs to the proxy, not to a file
	// the user now owns.
	dropMapKey(root, "networks")

	var sb strings.Builder
	enc := yaml.NewEncoder(&sb)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, nil, errf("eject_failed", "", "", "%v", err)
	}
	if err := enc.Close(); err != nil {
		return nil, nil, errf("eject_failed", "", "", "%v", err)
	}
	header := "# Written by `ob eject`. These services are yours now: Onebox will not\n" +
		"# regenerate them, and the project references this file instead.\n"
	return []byte(header + sb.String()), names, nil
}

func dropLabels(svc *yaml.Node) {
	labels := mapValue(svc, "labels")
	if labels == nil {
		return
	}
	var kept []*yaml.Node
	for i := 0; i+1 < len(labels.Content); i += 2 {
		k := labels.Content[i].Value
		if strings.HasPrefix(k, "ob.") || strings.HasPrefix(k, "traefik.") {
			continue
		}
		kept = append(kept, labels.Content[i], labels.Content[i+1])
	}
	if len(kept) == 0 {
		dropMapKey(svc, "labels")
		return
	}
	labels.Content = kept
}

func dropIngress(svc *yaml.Node) {
	nets := mapValue(svc, "networks")
	if nets == nil {
		return
	}
	var kept []*yaml.Node
	for _, n := range nets.Content {
		if n.Value == IngressNetwork || strings.HasPrefix(n.Value, "ob-") {
			continue
		}
		kept = append(kept, n)
	}
	if len(kept) <= 1 {
		// "default" alone carries no information once ingress is gone.
		dropMapKey(svc, "networks")
		return
	}
	nets.Content = kept
}

// repointProject rewrites the project file through its node tree, so comments
// and ordering survive. A workload's source becomes a reference to the ejected
// file; everything else about it is left exactly as written.
func repointProject(path, dest string, names []string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return errf("eject_failed", path, "", "cannot read the project file: %v", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return errf("eject_failed", path, "", "cannot read the project file: %v", err)
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}

	// A Compose-referenced workload is shaped by the file, not the declaration.
	// Leaving these behind would be worse than removing them: someone editing a
	// health check in the project file afterwards would see no effect and no
	// error. What stays is what still has meaning — the role, the routing the
	// overlay derives, and the intent fields.
	inert := []string{
		"image", "build", "command", "env", "env_files", "volumes", "ports",
		"health", "drain", "resources", "entrypoint", "user", "hostname",
		"working_dir", "init", "tty", "stdin_open", "extra_hosts", "labels",
		"logging", "persistence",
	}

	ejected := map[string]bool{}
	for _, n := range names {
		ejected[n] = true
	}

	if workloads := mapValue(root, "workloads"); workloads != nil {
		for i := 0; i+1 < len(workloads.Content); i += 2 {
			name, svc := workloads.Content[i].Value, workloads.Content[i+1]
			if !ejected[name] {
				continue
			}
			bh, bl := dropMapKey(svc, "build")
			ih, il := dropMapKey(svc, "image")
			for _, k := range inert {
				dropMapKey(svc, k)
			}
			setMapKey(svc, "compose", dest+"#"+name,
				firstNonEmpty(bh, ih), firstNonEmpty(bl, il))
		}
	} else {
		// Top-level shorthand: replace the source in place.
		bh, bl := dropMapKey(root, "build")
		ih, il := dropMapKey(root, "image")
		for _, k := range inert {
			dropMapKey(root, k)
		}
		setMapKey(root, "compose", dest+"#"+names[0],
			firstNonEmpty(bh, ih), firstNonEmpty(bl, il))
	}

	var sb strings.Builder
	enc := yaml.NewEncoder(&sb)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return errf("eject_failed", path, "", "%v", err)
	}
	if err := enc.Close(); err != nil {
		return errf("eject_failed", path, "", "%v", err)
	}

	tmp := path + ".ob-tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o600); err != nil {
		return errf("eject_failed", path, "", "%v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return errf("eject_failed", path, "", "%v", err)
	}
	return nil
}

func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// dropMapKey removes a key and returns whatever comments were attached to it,
// so a caller replacing one key with another can carry the author's note across
// rather than deleting it with the line it happened to sit on.
func dropMapKey(n *yaml.Node, key string) (head, line string) {
	if n == nil || n.Kind != yaml.MappingNode {
		return "", ""
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			k, v := n.Content[i], n.Content[i+1]
			head = firstNonEmpty(k.HeadComment, v.HeadComment)
			line = firstNonEmpty(k.LineComment, v.LineComment)
			n.Content = append(n.Content[:i], n.Content[i+2:]...)
			return head, line
		}
	}
	return "", ""
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func setMapKey(n *yaml.Node, key, value, head, line string) {
	if n == nil || n.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			n.Content[i+1].Value = value
			n.Content[i+1].Tag = "!!str"
			return
		}
	}
	k := &yaml.Node{Kind: yaml.ScalarNode, Value: key, HeadComment: head}
	v := &yaml.Node{Kind: yaml.ScalarNode, Value: value, Tag: "!!str", LineComment: line}
	n.Content = append(n.Content, k, v)
}
