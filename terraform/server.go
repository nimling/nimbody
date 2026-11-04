package terraform

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/nimling/nimbody/state"
)

const ConfigAttribute = "config"

type server struct {
	provider  Provider
	mu        sync.Mutex
	cli       command
	document  *state.Document
	resources map[string]resource
	problems  []*tfprotov6.Diagnostic
}

var providerSchema = &tfprotov6.Schema{Block: &tfprotov6.SchemaBlock{Attributes: []*tfprotov6.SchemaAttribute{
	{Name: ConfigAttribute, Type: tftypes.String, Optional: true, Description: "the named cli config to run with, as --config; the cli's own default when unset"},
}}}

func failed(summary string, err error) []*tfprotov6.Diagnostic {
	return []*tfprotov6.Diagnostic{{Severity: tfprotov6.DiagnosticSeverityError, Summary: summary, Detail: err.Error()}}
}

func (s *server) load(ctx context.Context, fresh bool) []*tfprotov6.Diagnostic {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.document != nil && !fresh {
		return s.problems
	}
	doc, err := s.cli.GetDocument(ctx)
	if err != nil {
		return failed("the "+s.provider.Name+" state schema cannot be read", err)
	}
	if doc.Format != state.FormatVersion {
		return failed("the "+s.provider.Name+" state format does not match", errors.New("the service speaks state format "+doc.Format+" and this provider reads "+state.FormatVersion+"; install the "+s.provider.Name+" release that matches the service"))
	}
	s.document = &doc
	s.resources = map[string]resource{}
	s.problems = nil
	for _, group := range doc.Groups {
		r, err := resourceOf(group)
		if err != nil {
			s.problems = append(s.problems, &tfprotov6.Diagnostic{Severity: tfprotov6.DiagnosticSeverityWarning, Summary: "group " + group.Name + " is not served", Detail: err.Error()})
			continue
		}
		s.resources[s.provider.Name+"_"+group.Name] = r
	}
	return s.problems
}

func (s *server) resource(ctx context.Context, name string) (resource, command, state.Document, []*tfprotov6.Diagnostic) {
	problems := s.load(ctx, false)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.document == nil {
		return resource{}, command{}, state.Document{}, problems
	}
	r, ok := s.resources[name]
	if !ok {
		return resource{}, command{}, state.Document{}, failed("no resource "+name, errors.New("the service has no state group for "+name))
	}
	return r, s.cli, *s.document, nil
}

func (s *server) GetMetadata(ctx context.Context, req *tfprotov6.GetMetadataRequest) (*tfprotov6.GetMetadataResponse, error) {
	resp := &tfprotov6.GetMetadataResponse{ServerCapabilities: &tfprotov6.ServerCapabilities{}}
	resp.Diagnostics = s.load(ctx, false)
	for _, name := range s.names() {
		resp.Resources = append(resp.Resources, tfprotov6.ResourceMetadata{TypeName: name})
	}
	return resp, nil
}

func (s *server) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.resources))
	for name := range s.resources {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *server) GetProviderSchema(ctx context.Context, req *tfprotov6.GetProviderSchemaRequest) (*tfprotov6.GetProviderSchemaResponse, error) {
	resp := &tfprotov6.GetProviderSchemaResponse{
		ServerCapabilities: &tfprotov6.ServerCapabilities{},
		Provider:           providerSchema,
		ResourceSchemas:    map[string]*tfprotov6.Schema{},
		DataSourceSchemas:  map[string]*tfprotov6.Schema{},
		Functions:          map[string]*tfprotov6.Function{},
	}
	resp.Diagnostics = s.load(ctx, false)
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, r := range s.resources {
		resp.ResourceSchemas[name] = r.schema
	}
	return resp, nil
}

func (s *server) GetResourceIdentitySchemas(ctx context.Context, req *tfprotov6.GetResourceIdentitySchemasRequest) (*tfprotov6.GetResourceIdentitySchemasResponse, error) {
	return &tfprotov6.GetResourceIdentitySchemasResponse{IdentitySchemas: map[string]*tfprotov6.ResourceIdentitySchema{}}, nil
}

func (s *server) ValidateProviderConfig(ctx context.Context, req *tfprotov6.ValidateProviderConfigRequest) (*tfprotov6.ValidateProviderConfigResponse, error) {
	return &tfprotov6.ValidateProviderConfigResponse{PreparedConfig: req.Config}, nil
}

func (s *server) ConfigureProvider(ctx context.Context, req *tfprotov6.ConfigureProviderRequest) (*tfprotov6.ConfigureProviderResponse, error) {
	resp := &tfprotov6.ConfigureProviderResponse{}
	if req.Config != nil {
		config, err := req.Config.Unmarshal(providerSchema.ValueType())
		if err != nil {
			resp.Diagnostics = failed("the provider config cannot be read", err)
			return resp, nil
		}
		name, _, err := jsonOf(at(config, tftypes.NewAttributePath().WithAttributeName(ConfigAttribute), tftypes.String))
		if err != nil {
			resp.Diagnostics = failed("the provider config cannot be read", err)
			return resp, nil
		}
		s.mu.Lock()
		s.cli.config, _ = name.(string)
		s.mu.Unlock()
	}
	resp.Diagnostics = s.load(ctx, true)
	return resp, nil
}

func (s *server) StopProvider(ctx context.Context, req *tfprotov6.StopProviderRequest) (*tfprotov6.StopProviderResponse, error) {
	return &tfprotov6.StopProviderResponse{}, nil
}

func (s *server) ValidateResourceConfig(ctx context.Context, req *tfprotov6.ValidateResourceConfigRequest) (*tfprotov6.ValidateResourceConfigResponse, error) {
	_, _, _, problems := s.resource(ctx, req.TypeName)
	return &tfprotov6.ValidateResourceConfigResponse{Diagnostics: problems}, nil
}

func (s *server) UpgradeResourceState(ctx context.Context, req *tfprotov6.UpgradeResourceStateRequest) (*tfprotov6.UpgradeResourceStateResponse, error) {
	resp := &tfprotov6.UpgradeResourceStateResponse{}
	r, _, _, problems := s.resource(ctx, req.TypeName)
	if problems != nil {
		resp.Diagnostics = problems
		return resp, nil
	}
	value, err := req.RawState.UnmarshalWithOpts(r.typ, tfprotov6.UnmarshalOpts{ValueFromJSONOpts: tftypes.ValueFromJSONOpts{IgnoreUndefinedAttributes: true}})
	if err != nil {
		resp.Diagnostics = failed("the stored state of "+req.TypeName+" cannot be read", err)
		return resp, nil
	}
	resp.UpgradedState, resp.Diagnostics = dynamic(r.typ, value)
	return resp, nil
}

func dynamic(typ tftypes.Type, value tftypes.Value) (*tfprotov6.DynamicValue, []*tfprotov6.Diagnostic) {
	out, err := tfprotov6.NewDynamicValue(typ, value)
	if err != nil {
		return nil, failed("a value cannot be encoded", err)
	}
	return &out, nil
}

func decoded(value *tfprotov6.DynamicValue, typ tftypes.Type) (tftypes.Value, error) {
	if value == nil {
		return tftypes.NewValue(typ, nil), nil
	}
	return value.Unmarshal(typ)
}

func keyOf(value tftypes.Value) string {
	key, _, _ := jsonOf(at(value, tftypes.NewAttributePath().WithAttributeName(KeyAttribute), tftypes.String))
	text, _ := key.(string)
	return text
}

func (s *server) ReadResource(ctx context.Context, req *tfprotov6.ReadResourceRequest) (*tfprotov6.ReadResourceResponse, error) {
	resp := &tfprotov6.ReadResourceResponse{}
	r, cli, _, problems := s.resource(ctx, req.TypeName)
	if problems != nil {
		resp.Diagnostics = problems
		return resp, nil
	}
	current, err := decoded(req.CurrentState, r.typ)
	if err != nil {
		resp.Diagnostics = failed("the state of "+req.TypeName+" cannot be read", err)
		return resp, nil
	}
	line, found, err := cli.GetRecord(ctx, r.group.Name, keyOf(current))
	if err != nil {
		resp.Diagnostics = failed("export of "+r.group.Name+" "+keyOf(current)+" failed", err)
		return resp, nil
	}
	if !found {
		resp.NewState, resp.Diagnostics = dynamic(r.typ, tftypes.NewValue(r.typ, nil))
		return resp, nil
	}
	value, err := r.value(line)
	if err != nil {
		resp.Diagnostics = failed(r.group.Name+" "+line.Key+" does not fit its schema", err)
		return resp, nil
	}
	resp.NewState, resp.Diagnostics = dynamic(r.typ, value)
	resp.Private = req.Private
	return resp, nil
}

func (s *server) PlanResourceChange(ctx context.Context, req *tfprotov6.PlanResourceChangeRequest) (*tfprotov6.PlanResourceChangeResponse, error) {
	resp := &tfprotov6.PlanResourceChangeResponse{}
	r, cli, doc, problems := s.resource(ctx, req.TypeName)
	if problems != nil {
		resp.Diagnostics = problems
		return resp, nil
	}
	proposed, err := decoded(req.ProposedNewState, r.typ)
	if err != nil {
		resp.Diagnostics = failed("the plan of "+req.TypeName+" cannot be read", err)
		return resp, nil
	}
	if proposed.IsNull() {
		resp.PlannedState = req.ProposedNewState
		return resp, nil
	}
	prior, err := decoded(req.PriorState, r.typ)
	if err != nil {
		resp.Diagnostics = failed("the state of "+req.TypeName+" cannot be read", err)
		return resp, nil
	}
	config, err := decoded(req.Config, r.typ)
	if err != nil {
		resp.Diagnostics = failed("the config of "+req.TypeName+" cannot be read", err)
		return resp, nil
	}
	planned, err := tftypes.Transform(proposed, func(path *tftypes.AttributePath, value tftypes.Value) (tftypes.Value, error) {
		switch {
		case matches(r.writeOnly, path):
			return tftypes.NewValue(value.Type(), nil), nil
		case matches(r.computed, path) && at(config, path, value.Type()).IsNull():
			if prior.IsNull() {
				return tftypes.NewValue(value.Type(), tftypes.UnknownValue), nil
			}
			return at(prior, path, value.Type()), nil
		}
		return value, nil
	})
	if err != nil {
		resp.Diagnostics = failed("the plan of "+req.TypeName+" cannot be made", err)
		return resp, nil
	}
	if resp.PlannedState, resp.Diagnostics = dynamic(r.typ, planned); resp.Diagnostics != nil {
		return resp, nil
	}
	if !prior.IsNull() {
		for _, path := range r.immutable {
			if !at(prior, path, tftypes.DynamicPseudoType).Equal(at(planned, path, tftypes.DynamicPseudoType)) {
				resp.RequiresReplace = append(resp.RequiresReplace, path)
			}
		}
	}
	if len(resp.RequiresReplace) > 0 || !planned.IsFullyKnown() || (!prior.IsNull() && prior.Equal(planned)) {
		return resp, nil
	}
	line, err := r.line(planned, config)
	if err != nil {
		resp.Diagnostics = failed("the plan of "+req.TypeName+" cannot be written as a record", err)
		return resp, nil
	}
	result, err := cli.PostImport(ctx, doc, &line, true)
	switch {
	case err != nil:
		resp.Diagnostics = []*tfprotov6.Diagnostic{{Severity: tfprotov6.DiagnosticSeverityWarning, Summary: "the dry run of " + r.group.Name + " " + line.Key + " failed", Detail: err.Error()}}
	case result.Outcome == state.OutcomeRefused:
		resp.Diagnostics = []*tfprotov6.Diagnostic{{Severity: tfprotov6.DiagnosticSeverityWarning, Summary: "the service would refuse " + r.group.Name + " " + line.Key + " as it stands", Detail: result.Error}}
	}
	return resp, nil
}

func (s *server) ApplyResourceChange(ctx context.Context, req *tfprotov6.ApplyResourceChangeRequest) (*tfprotov6.ApplyResourceChangeResponse, error) {
	resp := &tfprotov6.ApplyResourceChangeResponse{}
	r, cli, doc, problems := s.resource(ctx, req.TypeName)
	if problems != nil {
		resp.Diagnostics = problems
		return resp, nil
	}
	planned, err := decoded(req.PlannedState, r.typ)
	if err != nil {
		resp.Diagnostics = failed("the plan of "+req.TypeName+" cannot be read", err)
		return resp, nil
	}
	if planned.IsNull() {
		prior, err := decoded(req.PriorState, r.typ)
		if err != nil {
			resp.Diagnostics = failed("the state of "+req.TypeName+" cannot be read", err)
			return resp, nil
		}
		resp.Diagnostics = s.delete(ctx, cli, doc, r, keyOf(prior))
		if resp.Diagnostics == nil {
			resp.NewState, resp.Diagnostics = dynamic(r.typ, planned)
		}
		return resp, nil
	}
	config, err := decoded(req.Config, r.typ)
	if err != nil {
		resp.Diagnostics = failed("the config of "+req.TypeName+" cannot be read", err)
		return resp, nil
	}
	line, err := r.line(planned, config)
	if err != nil {
		resp.Diagnostics = failed("the plan of "+req.TypeName+" cannot be written as a record", err)
		return resp, nil
	}
	result, err := cli.PostImport(ctx, doc, &line, false)
	if err != nil {
		resp.Diagnostics = failed("import of "+r.group.Name+" "+line.Key+" failed", err)
		return resp, nil
	}
	if result.Outcome == state.OutcomeRefused {
		resp.Diagnostics = failed("the service refused "+r.group.Name+" "+line.Key, errors.New(result.Error))
		return resp, nil
	}
	written, found, err := cli.GetRecord(ctx, r.group.Name, line.Key)
	if err == nil && !found {
		err = errors.New("the service answers no record for it after the import")
	}
	if err != nil {
		resp.Diagnostics = failed("export of "+r.group.Name+" "+line.Key+" failed", err)
		return resp, nil
	}
	read, err := r.value(written)
	if err != nil {
		resp.Diagnostics = failed(r.group.Name+" "+line.Key+" does not fit its schema", err)
		return resp, nil
	}
	applied, err := tftypes.Transform(planned, func(path *tftypes.AttributePath, value tftypes.Value) (tftypes.Value, error) {
		if !value.IsKnown() {
			return at(read, path, value.Type()), nil
		}
		return value, nil
	})
	if err != nil {
		resp.Diagnostics = failed("the state of "+req.TypeName+" cannot be made", err)
		return resp, nil
	}
	resp.NewState, resp.Diagnostics = dynamic(r.typ, applied)
	return resp, nil
}

func (s *server) delete(ctx context.Context, cli command, doc state.Document, r resource, key string) []*tfprotov6.Diagnostic {
	result, err := cli.PostImport(ctx, doc, nil, false, r.group.Name+":"+key)
	if err != nil {
		return failed("delete of "+r.group.Name+" "+key+" failed", err)
	}
	if result.Outcome != state.OutcomeRefused {
		return nil
	}
	_, found, err := cli.GetRecord(ctx, r.group.Name, key)
	if err != nil {
		return failed("export of "+r.group.Name+" "+key+" failed", err)
	}
	if found {
		return failed("the service refused to delete "+r.group.Name+" "+key, errors.New(result.Error))
	}
	return nil
}

func (s *server) ImportResourceState(ctx context.Context, req *tfprotov6.ImportResourceStateRequest) (*tfprotov6.ImportResourceStateResponse, error) {
	resp := &tfprotov6.ImportResourceStateResponse{}
	r, _, _, problems := s.resource(ctx, req.TypeName)
	if problems != nil {
		resp.Diagnostics = problems
		return resp, nil
	}
	value, err := valueOf(map[string]any{KeyAttribute: req.ID}, r.typ)
	if err != nil {
		resp.Diagnostics = failed("key "+req.ID+" cannot be imported", err)
		return resp, nil
	}
	imported, problems := dynamic(r.typ, value)
	if problems != nil {
		resp.Diagnostics = problems
		return resp, nil
	}
	resp.ImportedResources = []*tfprotov6.ImportedResource{{TypeName: req.TypeName, State: imported}}
	return resp, nil
}

func unsupported(what string) []*tfprotov6.Diagnostic {
	return failed(what+" is not served", errors.New("the provider serves state groups as resources and nothing else"))
}

func (s *server) MoveResourceState(ctx context.Context, req *tfprotov6.MoveResourceStateRequest) (*tfprotov6.MoveResourceStateResponse, error) {
	return &tfprotov6.MoveResourceStateResponse{Diagnostics: unsupported("moving state")}, nil
}

func (s *server) UpgradeResourceIdentity(ctx context.Context, req *tfprotov6.UpgradeResourceIdentityRequest) (*tfprotov6.UpgradeResourceIdentityResponse, error) {
	return &tfprotov6.UpgradeResourceIdentityResponse{Diagnostics: unsupported("resource identity")}, nil
}

func (s *server) ValidateDataResourceConfig(ctx context.Context, req *tfprotov6.ValidateDataResourceConfigRequest) (*tfprotov6.ValidateDataResourceConfigResponse, error) {
	return &tfprotov6.ValidateDataResourceConfigResponse{Diagnostics: unsupported("data source " + req.TypeName)}, nil
}

func (s *server) ReadDataSource(ctx context.Context, req *tfprotov6.ReadDataSourceRequest) (*tfprotov6.ReadDataSourceResponse, error) {
	return &tfprotov6.ReadDataSourceResponse{Diagnostics: unsupported("data source " + req.TypeName)}, nil
}

func (s *server) GetFunctions(ctx context.Context, req *tfprotov6.GetFunctionsRequest) (*tfprotov6.GetFunctionsResponse, error) {
	return &tfprotov6.GetFunctionsResponse{Functions: map[string]*tfprotov6.Function{}}, nil
}

func (s *server) CallFunction(ctx context.Context, req *tfprotov6.CallFunctionRequest) (*tfprotov6.CallFunctionResponse, error) {
	return &tfprotov6.CallFunctionResponse{Error: &tfprotov6.FunctionError{Text: "function " + req.Name + " is not served"}}, nil
}

func (s *server) ValidateEphemeralResourceConfig(ctx context.Context, req *tfprotov6.ValidateEphemeralResourceConfigRequest) (*tfprotov6.ValidateEphemeralResourceConfigResponse, error) {
	return &tfprotov6.ValidateEphemeralResourceConfigResponse{Diagnostics: unsupported("ephemeral resource " + req.TypeName)}, nil
}

func (s *server) OpenEphemeralResource(ctx context.Context, req *tfprotov6.OpenEphemeralResourceRequest) (*tfprotov6.OpenEphemeralResourceResponse, error) {
	return &tfprotov6.OpenEphemeralResourceResponse{Diagnostics: unsupported("ephemeral resource " + req.TypeName)}, nil
}

func (s *server) RenewEphemeralResource(ctx context.Context, req *tfprotov6.RenewEphemeralResourceRequest) (*tfprotov6.RenewEphemeralResourceResponse, error) {
	return &tfprotov6.RenewEphemeralResourceResponse{Diagnostics: unsupported("ephemeral resource " + req.TypeName)}, nil
}

func (s *server) CloseEphemeralResource(ctx context.Context, req *tfprotov6.CloseEphemeralResourceRequest) (*tfprotov6.CloseEphemeralResourceResponse, error) {
	return &tfprotov6.CloseEphemeralResourceResponse{Diagnostics: unsupported("ephemeral resource " + req.TypeName)}, nil
}
