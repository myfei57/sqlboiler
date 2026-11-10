{{if and $.CloneGraph (not .Table.IsJoinTable) -}}
{{- $alias := .Aliases.Table .Table.Name -}}
{{- $m := $alias.UpSingular -}}
{{- $r := $alias.DownSingular -}}

// Clone{{$m}}Graph deep-copies o and every relationship already loaded on it
// using a fresh one-shot CloneGraphSession. It executes no SQL and triggers
// no loader or hook. A nil root returns a nil clone and no error.
func Clone{{$m}}Graph(o *{{$m}}) (*{{$m}}, error) {
	if o == nil {
		return nil, nil
	}

	return NewCloneGraphSession(CloneGraphBudget{}).Clone{{$m}}(o)
}

// Clone{{$m}} deep-copies o through s. Reusing one session for several roots
// keeps every shared node and shared mutable leaf identical between the
// clones. It executes no SQL and triggers no loader or hook.
func (s *CloneGraphSession) Clone{{$m}}(o *{{$m}}) (*{{$m}}, error) {
	if o == nil {
		return nil, nil
	}
	if err := s.checkFailed(); err != nil {
		return nil, err
	}

	var result *{{$m}}
	s.enqueue(0, func() error {
		return s.clone{{$m}}Node(o, 0, func(cp *{{$m}}) { result = cp })
	})

	if err := s.run(); err != nil {
		return nil, err
	}
	return result, nil
}

// Clone{{$m}}Slice deep-copies every element of os through s. A nil slice
// stays nil and an empty non-nil slice stays an empty non-nil slice; nil
// elements are preserved as nil.
func (s *CloneGraphSession) Clone{{$m}}Slice(os {{$m}}Slice) ({{$m}}Slice, error) {
	if os == nil {
		return nil, nil
	}
	if err := s.checkFailed(); err != nil {
		return nil, err
	}

	dst := make({{$m}}Slice, len(os))
	for i := range os {
		i, child := i, os[i]
		if child == nil {
			continue
		}

		s.enqueue(0, func() error {
			return s.clone{{$m}}Node(child, 0, func(cp *{{$m}}) { dst[i] = cp })
		})
	}

	if err := s.run(); err != nil {
		return nil, err
	}
	return dst, nil
}

// clone{{$m}}Node clones one model struct. The identity memo is consulted and
// updated before any allocation, which terminates cycles and preserves shared
// references. set is invoked with the clone (possibly from the memo) so the
// caller can wire it into a parent field even before the graph is finished.
func (s *CloneGraphSession) clone{{$m}}Node(src *{{$m}}, depth int, set func(*{{$m}})) error {
	if src == nil {
		set(nil)
		return nil
	}

	if cp, ok := s.{{$alias.DownSingular}}Memo[src]; ok {
		set(cp)
		return nil
	}

	if err := s.chargeNode(depth); err != nil {
		return err
	}

	dst := &{{$m}}{}
	s.{{$alias.DownSingular}}Memo[src] = dst
	set(dst)

	{{- range $column := .Table.Columns }}
	{{- $colAlias := $alias.Column $column.Name }}
	{{- $strategy := $.CloneGraphStrategy $column }}
	{{- if eq $strategy "value" }}
	dst.{{$colAlias}} = src.{{$colAlias}}
	{{- else }}
	{
		{{- if eq $strategy "bytes" }}
		cp, err := cloneGraphSlice(s, src.{{$colAlias}})
		if err != nil {
			return err
		}
		dst.{{$colAlias}} = cp
		{{- else if eq $strategy "json" }}
		cp, err := cloneGraphJSON(s, src.{{$colAlias}})
		if err != nil {
			return err
		}
		dst.{{$colAlias}} = cp
		{{- else if eq $strategy "nullbytes" }}
		dst.{{$colAlias}}.Valid = src.{{$colAlias}}.Valid
		cp, err := cloneGraphSlice(s, src.{{$colAlias}}.Bytes)
		if err != nil {
			return err
		}
		dst.{{$colAlias}}.Bytes = cp
		{{- else if eq $strategy "nulljson" }}
		dst.{{$colAlias}}.Valid = src.{{$colAlias}}.Valid
		cp, err := cloneGraphSlice(s, src.{{$colAlias}}.JSON)
		if err != nil {
			return err
		}
		dst.{{$colAlias}}.JSON = cp
		{{- else if eq $strategy "hstore" }}
		cp, err := cloneGraphDefinedMap(s, src.{{$colAlias}})
		if err != nil {
			return err
		}
		dst.{{$colAlias}} = cp
		{{- else if eq $strategy "slice" }}
		cp, err := cloneGraphDefinedSlice(s, src.{{$colAlias}})
		if err != nil {
			return err
		}
		dst.{{$colAlias}} = cp
		{{- else if eq $strategy "bytesarray" }}
		cp, err := cloneGraphDefinedBytesArray(s, src.{{$colAlias}})
		if err != nil {
			return err
		}
		dst.{{$colAlias}} = cp
		{{- else if eq $strategy "decimal" }}
		if src.{{$colAlias}}.Big == nil {
			dst.{{$colAlias}} = types.Decimal{}
		} else {
			cp, err := cloneGraphLeafOnce(s, reflect.ValueOf(src.{{$colAlias}}.Big).Pointer(), func() (types.Decimal, int) {
				d := types.CloneDecimal(src.{{$colAlias}})
				return d, len(src.{{$colAlias}}.Big.String())
			})
			if err != nil {
				return err
			}
			dst.{{$colAlias}} = cp
		}
		{{- else if eq $strategy "nulldecimal" }}
		if src.{{$colAlias}}.Big == nil {
			dst.{{$colAlias}} = types.NullDecimal{}
		} else {
			cp, err := cloneGraphLeafOnce(s, reflect.ValueOf(src.{{$colAlias}}.Big).Pointer(), func() (types.NullDecimal, int) {
				d := types.CloneNullDecimal(src.{{$colAlias}})
				return d, len(src.{{$colAlias}}.Big.String())
			})
			if err != nil {
				return err
			}
			dst.{{$colAlias}} = cp
		}
		{{- else if eq $strategy "decimalarray" }}
		if src.{{$colAlias}} == nil {
			dst.{{$colAlias}} = nil
		} else {
			cpArr, err := cloneGraphSlice(s, []types.Decimal(src.{{$colAlias}}))
			if err != nil {
				return err
			}
			for i := range cpArr {
				if src.{{$colAlias}}[i].Big == nil {
					cpArr[i] = types.Decimal{}
					continue
				}
				cp, err := cloneGraphLeafOnce(s, reflect.ValueOf(src.{{$colAlias}}[i].Big).Pointer(), func() (types.Decimal, int) {
					d := types.CloneDecimal(src.{{$colAlias}}[i])
					return d, len(src.{{$colAlias}}[i].Big.String())
				})
				if err != nil {
					return err
				}
				cpArr[i] = cp
			}
			dst.{{$colAlias}} = types.DecimalArray(cpArr)
		}
		{{- else if eq $strategy "path" }}
		if src.{{$colAlias}}.Points == nil {
			dst.{{$colAlias}} = pgeo.Path{Closed: src.{{$colAlias}}.Closed}
		} else {
			cp, err := cloneGraphDefinedSlice(s, src.{{$colAlias}}.Points)
			if err != nil {
				return err
			}
			dst.{{$colAlias}} = pgeo.Path{Points: cp, Closed: src.{{$colAlias}}.Closed}
		}
		{{- else if eq $strategy "nullpath" }}
		dst.{{$colAlias}}.Valid = src.{{$colAlias}}.Valid
		if src.{{$colAlias}}.Path.Points == nil {
			dst.{{$colAlias}}.Path = pgeo.Path{Closed: src.{{$colAlias}}.Path.Closed}
		} else {
			cp, err := cloneGraphDefinedSlice(s, src.{{$colAlias}}.Path.Points)
			if err != nil {
				return err
			}
			dst.{{$colAlias}}.Path = pgeo.Path{Points: cp, Closed: src.{{$colAlias}}.Path.Closed}
		}
		{{- else if eq $strategy "polygon" }}
		if src.{{$colAlias}} == nil {
			dst.{{$colAlias}} = nil
		} else {
			cp, err := cloneGraphDefinedSlice(s, []pgeo.Point(src.{{$colAlias}}))
			if err != nil {
				return err
			}
			dst.{{$colAlias}} = pgeo.Polygon(cp)
		}
		{{- else if eq $strategy "nullpolygon" }}
		dst.{{$colAlias}}.Valid = src.{{$colAlias}}.Valid
		if src.{{$colAlias}}.Polygon == nil {
			dst.{{$colAlias}}.Polygon = nil
		} else {
			cp, err := cloneGraphDefinedSlice(s, []pgeo.Point(src.{{$colAlias}}.Polygon))
			if err != nil {
				return err
			}
			dst.{{$colAlias}}.Polygon = pgeo.Polygon(cp)
		}
		{{- else if eq $strategy "adapter" }}
		cp, err := cloneGraphAdapter(s, src.{{$colAlias}})
		if err != nil {
			return err
		}
		dst.{{$colAlias}} = cp
		{{- else }}
		{{- /* fail template execution if the classifier ever returns an unknown strategy */ -}}
		{{- printf "clonegraph: unsupported copy strategy %q for column %q" $strategy $column.Name | fail }}
		{{- end }}
	}
	{{- end }}
	{{- end }}

	{{- if not .Table.IsView }}
	// A nil R means relationships were never loaded; keep that state instead
	// of inventing an empty relationship struct. No loader is ever invoked.
	if src.R != nil {
		dst.R = &{{$r}}R{}
		s.enqueue(depth+1, func() error {
			return s.clone{{$m}}REdges(src.R, dst.R, depth+1)
		})
	}
	{{- end }}

	return nil
}

{{- if not .Table.IsView }}

// clone{{$m}}REdges copies only the relationship content that was already
// loaded. Nil pointers, nil slices and empty non-nil slices keep their exact
// original state; foreign key column values are never read to fetch data.
func (s *CloneGraphSession) clone{{$m}}REdges(src *{{$r}}R, dst *{{$r}}R, depth int) error {
	{{- range .Table.FKeys }}
	{{- $ftable := $.Aliases.Table .ForeignTable }}
	{{- $relAlias := $alias.Relationship .Name }}
	if src.{{$relAlias.Foreign}} == nil {
		dst.{{$relAlias.Foreign}} = nil
	} else {
		if err := s.chargeEdge(depth); err != nil {
			return err
		}
		child := src.{{$relAlias.Foreign}}
		s.enqueue(depth, func() error {
			return s.clone{{$ftable.UpSingular}}Node(child, depth, func(cp *{{$ftable.UpSingular}}) {
				dst.{{$relAlias.Foreign}} = cp
			})
		})
	}
	{{- end }}

	{{- range .Table.ToOneRelationships }}
	{{- $ftable := $.Aliases.Table .ForeignTable }}
	{{- $relAlias := $ftable.Relationship .Name }}
	if src.{{$relAlias.Local}} == nil {
		dst.{{$relAlias.Local}} = nil
	} else {
		if err := s.chargeEdge(depth); err != nil {
			return err
		}
		child := src.{{$relAlias.Local}}
		s.enqueue(depth, func() error {
			return s.clone{{$ftable.UpSingular}}Node(child, depth, func(cp *{{$ftable.UpSingular}}) {
				dst.{{$relAlias.Local}} = cp
			})
		})
	}
	{{- end }}

	{{- range .Table.ToManyRelationships }}
	{{- $ftable := $.Aliases.Table .ForeignTable }}
	{{- $relAlias := $.Aliases.ManyRelationship .ForeignTable .Name .JoinTable .JoinLocalFKeyName }}
	if src.{{$relAlias.Local}} == nil {
		dst.{{$relAlias.Local}} = nil
	} else {
		dst.{{$relAlias.Local}} = make({{$ftable.UpSingular}}Slice, len(src.{{$relAlias.Local}}))
		for i := range src.{{$relAlias.Local}} {
			if err := s.chargeEdge(depth); err != nil {
				return err
			}
			i, child := i, src.{{$relAlias.Local}}[i]
			s.enqueue(depth, func() error {
				return s.clone{{$ftable.UpSingular}}Node(child, depth, func(cp *{{$ftable.UpSingular}}) {
					dst.{{$relAlias.Local}}[i] = cp
				})
			})
		}
	}
	{{- end }}

	return nil
}
{{- end }}
{{- end }}
