// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package interpreter provides functions to evaluate parsed expressions with
// the option to augment the evaluation with inputs and functions supplied at
// evaluation time.
package interpreter

import (
	"github.com/google/cel-go/common/packages"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	"github.com/google/cel-go/interpreter/functions"
)

// Interpreter generates a new Interpretable from a Program.
type Interpreter interface {
	// NewInterpretable returns an Interpretable from a Program.
	NewInterpretable(program Program) Interpretable
}

// Interpretable can accept a given Activation and produce a value along with
// an accompanying EvalState which can be used to inspect whether additional
// data might be necessary to complete the evaluation.
type Interpretable interface {
	// Eval an Activation to produce an output and EvalState.
	Eval(activation Activation) (ref.Value, EvalState)
}

type exprInterpreter struct {
	dispatcher   Dispatcher
	packager     packages.Packager
	typeProvider ref.TypeProvider
}

// NewInterpreter builds an Interpreter from a Dispatcher and TypeProvider
// which will be used throughout the Eval of all Interpretable instances
// gerenated from it.
func NewInterpreter(dispatcher Dispatcher,
	packager packages.Packager,
	typeProvider ref.TypeProvider) Interpreter {
	return &exprInterpreter{
		dispatcher:   dispatcher,
		packager:     packager,
		typeProvider: typeProvider}
}

// StandardInterpreter builds a Dispatcher and TypeProvider with support
// for all of the CEL builtins defined in the language definition.
func NewStandardIntepreter(packager packages.Packager,
	typeProvider ref.TypeProvider) Interpreter {
	dispatcher := NewDispatcher()
	dispatcher.Add(functions.StandardOverloads()...)
	return NewInterpreter(dispatcher, packager, typeProvider)
}

func (i *exprInterpreter) NewInterpretable(program Program) Interpretable {
	// program needs to be pruned with the TypeProvider
	evalState := NewEvalState(program.MaxInstructionId() + 1)
	program.Init(i.dispatcher, evalState)
	return &exprInterpretable{
		interpreter: i,
		program:     program,
		state:       evalState}
}

type exprInterpretable struct {
	interpreter *exprInterpreter
	program     Program
	state       MutableEvalState
}

func (i *exprInterpretable) Eval(activation Activation) (ref.Value, EvalState) {
	// register machine-like evaluation of the program with the given activation.
	currActivation := activation
	stepper := i.program.Begin()
	var resultId int64
	for step, hasNext := stepper.Next(); hasNext; step, hasNext = stepper.Next() {
		resultId = step.GetId()
		switch step.(type) {
		case *IdentExpr:
			i.evalIdent(step.(*IdentExpr), currActivation)
		case *SelectExpr:
			i.evalSelect(step.(*SelectExpr), currActivation)
		case *CallExpr:
			i.evalCall(step.(*CallExpr), currActivation)
		case *CreateListExpr:
			i.evalCreateList(step.(*CreateListExpr))
		case *CreateMapExpr:
			i.evalCreateMap(step.(*CreateMapExpr))
		case *CreateObjectExpr:
			i.evalCreateType(step.(*CreateObjectExpr))
		case *MovInst:
			i.evalMov(step.(*MovInst))
			// Special instruction for modifying the program cursor
		case *JumpInst:
			jmpExpr := step.(*JumpInst)
			if jmpExpr.OnCondition(i.state) {
				if !stepper.JumpCount(jmpExpr.Count) {
					// TODO: Error, the jump count should never exceed the
					// program length.
					panic("jumped too far")
				}
			}
			// Special instructions for modifying the activation stack
		case *PushScopeInst:
			pushScope := step.(*PushScopeInst)
			scopeDecls := pushScope.Declarations
			childActivaton := make(map[string]interface{})
			for key, declId := range scopeDecls {
				childActivaton[key] = func() interface{} {
					return i.value(declId)
				}
			}
			currActivation = NewHierarchicalActivation(currActivation, NewActivation(childActivaton))
		case *PopScopeInst:
			currActivation = currActivation.Parent()
		}
	}
	result := i.value(resultId)
	if result == nil {
		result, _ = i.state.OnlyValue()
	}
	return result, i.state
}

func (i *exprInterpretable) evalConst(constExpr *ConstExpr) {
	i.setValue(constExpr.GetId(), constExpr.Value)
}

func (i *exprInterpretable) evalIdent(idExpr *IdentExpr, currActivation Activation) {
	// TODO: Refactor this code for sharing.
	if result, found := currActivation.ResolveName(idExpr.Name); found {
		i.setValue(idExpr.GetId(), result)
	} else if idVal, found := i.interpreter.typeProvider.FindIdent(idExpr.Name); found {
		i.setValue(idExpr.GetId(), idVal)
	} else {
		i.setValue(idExpr.GetId(), types.Unknown{idExpr.Id})
	}
}

func (i *exprInterpretable) evalSelect(selExpr *SelectExpr, currActivation Activation) {
	operand := i.value(selExpr.Operand)
	if !operand.Type().HasTrait(traits.IndexerType) {
		if types.IsUnknown(operand) {
			i.resolveUnknown(operand.(types.Unknown), selExpr, currActivation)
		} else {
			i.setValue(selExpr.Operand, types.NewErr("invalid operand in select"))
		}
		return
	}
	fieldValue := operand.(traits.Indexer).Get(types.String(selExpr.Field))
	i.setValue(selExpr.GetId(), fieldValue)
}

// resolveUnknown attempts to resolve a qualified name from a select expression
// which may have generated unknown values during the course of execution if
// the expression was not type-checked and the select, in fact, refers to a
// qualified identifier name instead of a series of field selections.
func (i *exprInterpretable) resolveUnknown(unknown types.Unknown,
	selExpr *SelectExpr,
	currActivation Activation) {
	if object, found := currActivation.ResolveReference(selExpr.Id); found {
		i.setValue(selExpr.Id, object)
		return
	}
	validIdent := true
	identifier := selExpr.Field
	for _, arg := range unknown {
		inst := i.program.GetInstruction(arg)
		switch inst.(type) {
		case *IdentExpr:
			identifier = inst.(*IdentExpr).Name + "." + identifier
		case *SelectExpr:
			identifier = inst.(*SelectExpr).Field + "." + identifier
		default:
			argVal := i.value(arg)
			if argVal.Type() == types.StringType {
				identifier = string(argVal.(types.String)) + "." + identifier
			} else {
				validIdent = false
				break
			}
		}
	}
	if !validIdent {
		return
	}
	pkg := i.interpreter.packager
	tp := i.interpreter.typeProvider
	for _, id := range pkg.ResolveCandidateNames(identifier) {
		if object, found := currActivation.ResolveName(id); found {
			i.setValue(selExpr.Id, object)
			return
		}
		if identVal, found := tp.FindIdent(id); found {
			i.setValue(selExpr.Id, identVal)
			return
		}
	}
	i.setValue(selExpr.Id, append(types.Unknown{selExpr.Id}, unknown...))
}

func (i *exprInterpretable) evalCall(callExpr *CallExpr, currActivation Activation) {
	argVals := make([]ref.Value, len(callExpr.Args), len(callExpr.Args))
	for idx, argId := range callExpr.Args {
		argVals[idx] = i.value(argId)
		if callExpr.Strict && (types.IsError(argVals[idx]) || types.IsUnknown(argVals[idx])) {
			i.setValue(callExpr.GetId(), argVals[idx])
			return
		}
	}
	ctx := &CallContext{
		call:       callExpr,
		activation: currActivation,
		args:       argVals,
		metadata:   i.program.Metadata()}
	result := i.interpreter.dispatcher.Dispatch(ctx)
	i.setValue(callExpr.GetId(), result)
}

func (i *exprInterpretable) evalCreateList(listExpr *CreateListExpr) {
	elements := make([]ref.Value, len(listExpr.Elements))
	for idx, elementId := range listExpr.Elements {
		elem := i.value(elementId)
		if types.IsError(elem.Type()) || types.IsUnknown(elem.Type()) {
			i.setValue(listExpr.GetId(), elem)
			return
		}
		elements[idx] = i.value(elementId)
	}
	adaptingList := types.NewDynamicList(elements)
	i.setValue(listExpr.GetId(), adaptingList)
}

func (i *exprInterpretable) evalCreateMap(mapExpr *CreateMapExpr) {
	entries := make(map[ref.Value]ref.Value)
	for keyId, valueId := range mapExpr.KeyValues {
		key := i.value(keyId)
		if types.IsError(key.Type()) || types.IsUnknown(key.Type()) {
			i.setValue(mapExpr.GetId(), key)
			return
		}
		val := i.value(valueId)
		if types.IsError(val.Type()) || types.IsUnknown(val.Type()) {
			i.setValue(mapExpr.GetId(), val)
			return
		}
		entries[key] = val
	}
	adaptingMap := types.NewDynamicMap(entries)
	i.setValue(mapExpr.GetId(), adaptingMap)
}

func (i *exprInterpretable) evalCreateType(objExpr *CreateObjectExpr) {
	fields := make(map[string]ref.Value)
	for field, valueId := range objExpr.FieldValues {
		val := i.value(valueId)
		if types.IsError(val) || types.IsUnknown(val) {
			i.setValue(objExpr.GetId(), val)
			return
		}
		fields[field] = val
	}
	i.setValue(objExpr.GetId(), i.newValue(objExpr.Name, fields))
}

func (i *exprInterpretable) evalMov(movExpr *MovInst) {
	i.setValue(movExpr.ToExprId, i.value(movExpr.GetId()))
}

func (i *exprInterpretable) value(id int64) ref.Value {
	if object, found := i.state.Value(id); found {
		return object
	}
	return types.Unknown{id}
}

func (i *exprInterpretable) setValue(id int64, value ref.Value) {
	i.state.SetValue(id, value)
}

func (i *exprInterpretable) newValue(typeName string,
	fields map[string]ref.Value) ref.Value {
	pkg := i.interpreter.packager
	tp := i.interpreter.typeProvider
	for _, qualifiedTypeName := range pkg.ResolveCandidateNames(typeName) {
		if _, found := tp.FindType(qualifiedTypeName); found {
			typeName = qualifiedTypeName
			break
		}
	}
	return i.interpreter.typeProvider.NewValue(typeName, fields)
}
