package gateway

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/hingst-it/panko/internal/jsonx"
)

type Object = jsonx.Object
type Resource struct {
	Group, Version, Kind, Plural string
	Schema                       Object
}
type Route struct{ Version, Plural string }

func Resources(crds []any) (map[Route]Resource, error) {
	result := make(map[Route]Resource)
	for _, item := range crds {
		crd := jsonx.Obj(item)
		if jsonx.Str(jsonx.Obj(jsonx.Obj(crd["metadata"])["labels"])["platform.example.com/expose"]) != "true" {
			continue
		}
		spec := jsonx.Obj(crd["spec"])
		if spec["scope"] != "Namespaced" {
			continue
		}
		for _, item := range jsonx.Array(spec["versions"]) {
			v := jsonx.Obj(item)
			if v["served"] != true {
				continue
			}
			root := jsonx.Obj(jsonx.Obj(v["schema"])["openAPIV3Schema"])
			if _, ok := jsonx.Obj(root["properties"])["spec"]; !ok {
				continue
			}
			names := jsonx.Obj(spec["names"])
			r := Resource{jsonx.Str(spec["group"]), jsonx.Str(v["name"]), jsonx.Str(names["kind"]), jsonx.Str(names["plural"]), root}
			key := Route{r.Version, r.Plural}
			if _, ok := result[key]; ok {
				return nil, fmt.Errorf("Ambiguous exposed API route: ('%s', '%s')", key.Version, key.Plural)
			}
			result[key] = r
		}
	}
	return result, nil
}

func JSONSchema(source any) any {
	switch x := source.(type) {
	case []any:
		result := make([]any, len(x))
		for i, v := range x {
			result[i] = JSONSchema(v)
		}
		return result
	case map[string]any:
		result := Object{}
		for k, v := range x {
			if k != "nullable" {
				result[k] = JSONSchema(v)
			}
		}
		if x["nullable"] == true {
			return Object{"anyOf": []any{result, Object{"type": "null"}}}
		}
		return result
	default:
		return source
	}
}

func PatchSchema(source Object) Object {
	r := jsonx.Clone(source)
	if r["type"] == "object" {
		required := map[string]bool{}
		for _, k := range jsonx.Array(r["required"]) {
			required[jsonx.Str(k)] = true
		}
		delete(r, "required")
		for k, v := range jsonx.Obj(r["properties"]) {
			partial := PatchSchema(jsonx.Obj(v))
			if required[k] {
				jsonx.Obj(r["properties"])[k] = partial
			} else {
				jsonx.Obj(r["properties"])[k] = Object{"anyOf": []any{partial, Object{"type": "null"}}}
			}
		}
	}
	delete(r, "default")
	return r
}

var identifierPattern = regexp.MustCompile(`[^a-zA-Z0-9_]`)

func ref(name string) Object    { return Object{"$ref": "#/components/schemas/" + name} }
func content(schema any) Object { return Object{"application/json": Object{"schema": schema}} }

func OpenAPI(discovered map[Route]Resource) Object {
	schemas := Object{"Error": Object{"type": "object", "required": []any{"message", "errors"}, "properties": Object{
		"message": Object{"type": "string"}, "errors": Object{"type": "array", "items": Object{"type": "object", "required": []any{"field", "message"},
			"properties": Object{"field": Object{"type": "string"}, "message": Object{"type": "string"}}}}}}}
	paths := Object{}
	keys := make([]Route, 0, len(discovered))
	for key := range discovered {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Version == keys[j].Version {
			return keys[i].Plural < keys[j].Plural
		}
		return keys[i].Version < keys[j].Version
	})
	for _, key := range keys {
		r := discovered[key]
		root := jsonx.Obj(JSONSchema(r.Schema))
		properties := jsonx.Obj(root["properties"])
		id := identifierPattern.ReplaceAllString(r.Group+"_"+r.Version+"_"+r.Kind, "_")
		name := Object{"type": "string", "description": "Resource name, unique within this team."}
		description, ok := root["description"]
		if !ok {
			description = r.Kind
		}
		request := Object{"type": "object", "description": description, "required": []any{"name", "spec"}, "additionalProperties": false,
			"properties": Object{"name": name, "spec": properties["spec"]}}
		response := jsonx.Clone(request)
		if status, ok := properties["status"]; ok {
			jsonx.Obj(response["properties"])["status"] = status
		}
		schemas[id+"Create"] = request
		schemas[id+"Patch"] = Object{"type": "object", "additionalProperties": false, "properties": Object{"spec": PatchSchema(jsonx.Obj(properties["spec"]))}}
		schemas[id] = response
		schemas[id+"List"] = Object{"type": "array", "items": ref(id)}
		collection := "/" + r.Version + "/teams/{team}/" + r.Plural
		for _, item := range []bool{false, true} {
			path := collection
			methods := []string{"get", "post"}
			suffix := "_collection"
			parameters := []any{Object{"name": "team", "in": "path", "required": true, "schema": Object{"type": "string"}, "description": "Team namespace authorized by the bearer token."}}
			if item {
				path += "/{name}"
				methods = []string{"get", "patch", "delete"}
				suffix = "_item"
				parameters = append(parameters, Object{"name": "name", "in": "path", "required": true, "schema": name})
			}
			operations := Object{}
			for _, method := range methods {
				code, label := "200", "Success"
				if method == "post" {
					code = "201"
				}
				if method == "delete" {
					code, label = "202", "Accepted"
				}
				responses := Object{code: Object{"description": label}}
				for _, e := range []struct{ code, label string }{{"401", "Token required"}, {"403", "Forbidden"}, {"404", "Not found"}, {"409", "Conflict"}, {"422", "API validation failed"}, {"503", "Kubernetes unavailable"}} {
					responses[e.code] = Object{"description": e.label, "content": content(ref("Error"))}
				}
				op := Object{"operationId": method + "_" + id + suffix, "tags": []any{r.Kind}, "summary": strings.ToUpper(method) + " " + r.Kind,
					"parameters": parameters, "security": []any{Object{"TeamToken": []any{}}}, "responses": responses}
				if method != "delete" {
					model := id
					if method == "get" && !item {
						model += "List"
					}
					jsonx.Obj(responses[code])["content"] = content(ref(model))
				}
				if method == "post" || method == "patch" {
					model, media := id+"Create", "application/json"
					if method == "patch" {
						model, media = id+"Patch", "application/merge-patch+json"
					}
					op["requestBody"] = Object{"required": true, "content": Object{media: Object{"schema": ref(model)}}}
				}
				operations[method] = op
			}
			paths[path] = operations
		}
		responses := Object{"202": Object{"description": "Retry requested", "content": content(ref(id))}}
		for _, e := range []struct{ code, label string }{{"401", "Token required"}, {"403", "Forbidden"}, {"404", "Not found"}, {"409", "Nothing to retry or resource changed"}, {"503", "Kubernetes unavailable"}} {
			responses[e.code] = Object{"description": e.label, "content": content(ref("Error"))}
		}
		paths[collection+"/{name}/retry"] = Object{"post": Object{"operationId": "retry_" + id + "_item", "tags": []any{r.Kind}, "summary": "Retry the failed execution of a " + r.Kind,
			"parameters": []any{Object{"name": "team", "in": "path", "required": true, "schema": Object{"type": "string"}, "description": "Team namespace authorized by the bearer token."},
				Object{"name": "name", "in": "path", "required": true, "schema": name}},
			"security": []any{Object{"TeamToken": []any{}}}, "responses": responses}}
	}
	return Object{"openapi": "3.1.0", "info": Object{"title": "Panko infrastructure API", "version": "1.0.0",
		"description": "Generated from exposed Kubernetes CRDs. The API server evaluates schema and CEL rules."}, "paths": paths,
		"components": Object{"schemas": schemas, "securitySchemes": Object{"TeamToken": Object{"type": "http", "scheme": "bearer"}}}}
}
