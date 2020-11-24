package cmd

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/errors"
	"sigs.k8s.io/structured-merge-diff/v4/fieldpath"
	"sigs.k8s.io/structured-merge-diff/v4/value"
)

const timeLayout = "2006-01-02 15:04:05 -0700"

var (
	bytesNull        = []byte("null\n")
	bytesEmptyObject = []byte("{}\n")
	bytesEmptyList   = []byte("[]\n")
)

type Marshaller struct {
	emptyInfo string
}

type KeyWithNode struct {
	Node *Node
	Key  value.FieldList
}

type ManagerInfo struct {
	Manager   string
	Operation string
	Time      *metav1.Time
}

func (i *ManagerInfo) String() string {
	return fmt.Sprintf("%s (%s %s) ", i.Manager, i.Operation, i.Time.Format(timeLayout))
}

type Node struct {
	// field name -> child node
	Fields map[string]*Node
	// fieldList ID -> {fieldList, child node}
	Keys   map[string]*KeyWithNode
	Parent *Node

	Info *ManagerInfo
}

func (n *Node) print(lvl int, infoLength int) {
	var keys []string
	for key := range n.Fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		child := n.Fields[key]
		if child.Info != nil {
			fmt.Printf("%s", child.Info.String())
		} else {
			fmt.Print(strings.Repeat(" ", infoLength))
		}
		fmt.Print(strings.Repeat("  ", lvl))
		fmt.Printf(" f:%s\n", key)
		child.print(lvl+1, infoLength)
	}

	keys = keys[:0]
	for key := range n.Keys {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		s := n.Keys[key]
		child := s.Node
		if child.Info != nil {
			fmt.Printf("%s", child.Info.String())
		} else {
			fmt.Print(strings.Repeat(" ", infoLength))
		}
		fmt.Print(strings.Repeat("  ", lvl))
		fmt.Printf(" k:%s\n", fieldListID(s.Key))
		child.print(lvl+1, infoLength)
	}
}

func buildTree(managedFields []metav1.ManagedFieldsEntry, mgrMaxLength, opMaxLength int) (*Node, error) {
	root := &Node{}

	for _, field := range managedFields {
		manager := field.Manager
		operation := string(field.Operation)
		if len(manager) < mgrMaxLength {
			manager = appendSpace(manager, mgrMaxLength)
		}
		if len(operation) < opMaxLength {
			operation = appendSpace(operation, opMaxLength)
		}
		info := ManagerInfo{
			Manager:   manager,
			Operation: operation,
			Time:      field.Time,
		}

		s, err := fieldsToSet(*field.FieldsV1)
		if err != nil {
			return nil, err
		}

		var errList []error
		s.Iterate(func(path fieldpath.Path) {
			cur := root
			l := len(path)
			for i, ele := range path {
				isLeaf := i == l-1
				switch {
				case ele.FieldName != nil:
					name := *ele.FieldName
					if cur.Fields == nil {
						cur.Fields = map[string]*Node{}
					}
					if cur.Fields[name] == nil {
						cur.Fields[name] = &Node{Parent: cur}
					}
					cur = cur.Fields[name]
				case ele.Key != nil:
					name := fieldListID(*ele.Key)
					if cur.Keys == nil {
						cur.Keys = map[string]*KeyWithNode{}
					}
					if cur.Keys[name] == nil {
						cur.Keys[name] = &KeyWithNode{
							Node: &Node{Parent: cur},
							Key:  *ele.Key,
						}
					}
					cur = cur.Keys[name].Node
				default:
					errList = append(errList, fmt.Errorf("unknown element: %#v", ele))
					continue
				}
				if isLeaf {
					cur.Info = &info
				}
			}
		})
		if len(errList) > 0 {
			return nil, errors.NewAggregate(errList)
		}
	}
	return root, nil
}

func MarshalMetaObject(obj metav1.Object) ([]byte, error) {
	var m Marshaller
	return m.marshalMetaObject(obj)
}

func (m *Marshaller) marshalMetaObject(obj metav1.Object) ([]byte, error) {
	managedFields := obj.GetManagedFields()
	if len(managedFields) == 0 {
		return nil, fmt.Errorf(".metadata.managedFields is empty")
	}

	var (
		mgrMaxLength, opMaxLength int
	)
	for _, field := range managedFields {
		if len(field.Manager) > mgrMaxLength {
			mgrMaxLength = len(field.Manager)
		}
		if len(field.Operation) > opMaxLength {
			opMaxLength = len(field.Operation)
		}
	}

	root, err := buildTree(managedFields, mgrMaxLength, opMaxLength)
	if err != nil {
		return nil, err
	}

	// manager + space + bracket + operation + space + time + bracket + space
	infoLength := mgrMaxLength + 1 + 1 + opMaxLength + 1 + len(timeLayout) + 1 + 1
	m.emptyInfo = strings.Repeat(" ", infoLength)

	obj.SetManagedFields(nil)
	unsObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	err = m.marshalMapWithCtx(Context{
		Level:   0,
		NewLine: true,
		Node:    root,
	}, unsObj, &buf)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (m *Marshaller) marshalMapWithCtx(ctx Context, o map[string]interface{}, w io.Writer) error {
	if o == nil {
		w.Write(bytesNull)
		return nil
	}
	if len(o) == 0 {
		w.Write(bytesEmptyObject)
		return nil
	}

	// make the result predictable
	keys := make([]string, 0, len(o))
	for key := range o {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	root := ctx.Node
	for i, key := range keys {
		val := o[key]
		var child *Node
		if i == 0 && !ctx.NewLine {
			writeString(w, toYAMLString(key))
		} else {
			var ok bool
			child, ok = root.Fields[key]
			if ok {
				info := getInfoOr(child, m.emptyInfo)
				writeString(w, info)
			} else {
				info := getInfoOr(root, m.emptyInfo)
				writeString(w, info)
			}
			writeIndent(w, ctx.Level)
			writeString(w, toYAMLString(key))
		}
		writeBytes(w, ':')

		if child == nil {
			child = root
		}
		switch actual := val.(type) {
		case map[string]interface{}:
			if len(actual) == 0 {
				writeBytes(w, ' ')
			} else {
				writeBytes(w, '\n')
			}
			m.marshalMapWithCtx(Context{
				NewLine: true,
				Level:   ctx.Level + 1,
				Node:    child,
			}, actual, w)
			continue
		case []interface{}:
			if len(actual) == 0 {
				writeBytes(w, ' ')
			} else {
				writeBytes(w, '\n')
			}
			m.marshalListWithCtx(ctx.WithNewLine(true).WithNode(child), actual, w)
			continue
		}

		writeBytes(w, ' ')
		switch actual := val.(type) {
		case string:
			writeString(w, toYAMLStringValueln(m.emptyInfo, actual, ctx.Level+1))
		case nil:
			w.Write(bytesNull)
		default:
			writeString(w, formatBasicType(val)+"\n")
		}
	}
	return nil
}

func (m *Marshaller) marshalListWithCtx(ctx Context, o []interface{}, w io.Writer) error {
	if o == nil {
		w.Write(bytesNull)
		return nil
	}
	if len(o) == 0 {
		w.Write(bytesEmptyList)
		return nil
	}

	root := ctx.Node
	prefix := getInfoOr(root, m.emptyInfo)
	for i, val := range o {
		switch actual := val.(type) {
		case map[string]interface{}:
			child := root // fallback to root
			for _, s := range root.Keys {
				if fieldListMatchObject(s.Key, actual) {
					child = s.Node
				}
			}
			prefix := getInfoOr(child, m.emptyInfo)
			writeString(w, prefix)
			writeIndent(w, ctx.Level)
			writeBytes(w, '-', ' ')
			m.marshalMapWithCtx(Context{
				Level:   ctx.Level + 1,
				NewLine: false,
				Node:    child,
			}, actual, w)
			continue
		case []interface{}:
			if i != 0 || ctx.NewLine {
				writeString(w, prefix)
				writeIndent(w, ctx.Level)
				writeBytes(w, '-', ' ')
			}
			m.marshalListWithCtx(ctx.WithNewLine(false).WithLevel(ctx.Level+1), actual, w)
			continue
		}

		writeString(w, prefix)
		writeIndent(w, ctx.Level)
		writeBytes(w, '-', ' ')
		switch actual := val.(type) {
		case string:
			writeString(w, toYAMLStringValueln(m.emptyInfo, actual, ctx.Level+1))
		case nil:
			w.Write(bytesNull)
		default:
			writeString(w, formatBasicType(val)+"\n")
		}
	}
	return nil
}
