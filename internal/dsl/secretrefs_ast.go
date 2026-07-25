package dsl

import (
	"errors"
	"fmt"
	"text/template"
	"text/template/parse"
)

var errDynamicSecretName = errors.New(
	"dynamic secret name must be resolved from a parameter before execution",
)

func validateSecretReferences(tpl string) error {
	parsed, err := template.New("").
		Funcs(funcMap).
		Option("missingkey=zero").
		Parse(normalizeSecretsRefs(tpl))
	if err != nil {
		return fmt.Errorf("parse secret references: %w", err)
	}
	for _, defined := range parsed.Templates() {
		if defined.Tree == nil || defined.Tree.Root == nil {
			continue
		}
		if err := validateSecretReferenceNode(defined.Tree.Root); err != nil {
			return err
		}
	}
	return nil
}

func validateSecretReferenceNode(node parse.Node) error {
	if node == nil {
		return nil
	}
	switch node := node.(type) {
	case *parse.ListNode:
		if node == nil {
			return nil
		}
		for _, child := range node.Nodes {
			if err := validateSecretReferenceNode(child); err != nil {
				return err
			}
		}
	case *parse.ActionNode:
		return validateSecretReferenceNode(node.Pipe)
	case *parse.IfNode:
		return validateSecretReferenceBranch(&node.BranchNode)
	case *parse.WithNode:
		return validateSecretReferenceBranch(&node.BranchNode)
	case *parse.RangeNode:
		return validateSecretReferenceBranch(&node.BranchNode)
	case *parse.TemplateNode:
		return validateSecretReferenceNode(node.Pipe)
	case *parse.PipeNode:
		if node == nil {
			return nil
		}
		for _, command := range node.Cmds {
			if err := validateSecretReferenceCommand(command); err != nil {
				return err
			}
		}
	case *parse.CommandNode:
		return validateSecretReferenceCommand(node)
	case *parse.FieldNode:
		if isAllowedDirectSecretValue(node) {
			name := node.Ident[1]
			if err := ValidateSecretName(name); err != nil {
				return fmt.Errorf("secret name %q %w", name, err)
			}
			return nil
		}
		if containsReservedSecrets(node.Ident) {
			return errDynamicSecretName
		}
	case *parse.VariableNode:
		if containsReservedSecrets(node.Ident) {
			return errDynamicSecretName
		}
	case *parse.ChainNode:
		if containsReservedSecrets(node.Field) {
			return errDynamicSecretName
		}
		return validateSecretReferenceNode(node.Node)
	}
	return nil
}

func validateSecretReferenceBranch(branch *parse.BranchNode) error {
	if err := validateSecretReferenceNode(branch.Pipe); err != nil {
		return err
	}
	if err := validateSecretReferenceNode(branch.List); err != nil {
		return err
	}
	return validateSecretReferenceNode(branch.ElseList)
}

func validateSecretReferenceCommand(command *parse.CommandNode) error {
	if isCanonicalSecretIndex(command) {
		return nil
	}
	for _, argument := range command.Args {
		if err := validateSecretReferenceNode(argument); err != nil {
			return err
		}
	}
	return nil
}

func isCanonicalSecretIndex(command *parse.CommandNode) bool {
	if len(command.Args) != 3 {
		return false
	}
	identifier, ok := command.Args[0].(*parse.IdentifierNode)
	if !ok || identifier.Ident != "index" {
		return false
	}
	receiver, ok := command.Args[1].(*parse.FieldNode)
	if !ok || len(receiver.Ident) != 1 || receiver.Ident[0] != "Secrets" {
		return false
	}
	_, ok = command.Args[2].(*parse.StringNode)
	return ok
}

func isAllowedDirectSecretValue(field *parse.FieldNode) bool {
	return len(field.Ident) == 2 && field.Ident[0] == "Secrets"
}

func containsReservedSecrets(fields []string) bool {
	for _, field := range fields {
		if field == "Secrets" {
			return true
		}
	}
	return false
}
