package list

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"path"
	"slices"
	"sync"
	"time"

	stderrors "errors"

	"github.com/spf13/cobra"
	"github.com/stackitcloud/stackit-cli/internal/cmd/params"
	"github.com/stackitcloud/stackit-cli/internal/pkg/args"
	"github.com/stackitcloud/stackit-cli/internal/pkg/auth"
	"github.com/stackitcloud/stackit-cli/internal/pkg/errors"
	"github.com/stackitcloud/stackit-cli/internal/pkg/examples"
	"github.com/stackitcloud/stackit-cli/internal/pkg/flags"
	"github.com/stackitcloud/stackit-cli/internal/pkg/globalflags"
	"github.com/stackitcloud/stackit-cli/internal/pkg/print"
	authorizationclient "github.com/stackitcloud/stackit-cli/internal/pkg/services/authorization/client"
	resourcemanagerclient "github.com/stackitcloud/stackit-cli/internal/pkg/services/resourcemanager/client"
	"github.com/stackitcloud/stackit-cli/internal/pkg/tables"
	"github.com/stackitcloud/stackit-cli/internal/pkg/utils"
	"github.com/stackitcloud/stackit-sdk-go/services/authorization"
	"github.com/stackitcloud/stackit-sdk-go/services/resourcemanager"
	"golang.org/x/sync/errgroup"
)

const (
	memberFlag            = "member"
	creationTimeAfterFlag = "creation-time-after"
	folderIdFlag          = "folder-id"
	folderNameFlag        = "folder-name"
	organizationIdFlag    = "organization-id"
	organizationNameFlag  = "organization-name"

	creationTimeAfterFormat = time.RFC3339
)

type inputModel struct {
	*globalflags.GlobalFlagModel
	OrganizationId   []string
	OrganizationName *string
	FolderId         []string
	FolderName       *string

	Member            *string
	CreationTimeAfter *time.Time
	Organization      *string
}

func NewCmd(params *params.CmdParams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Lists STACKIT projects",
		Long:  "Lists all STACKIT projects that match certain criteria.",
		Args:  args.NoArgs,
		Example: examples.Build(
			examples.NewExample(
				`List all STACKIT projects that the authenticated user or service account is a member of`,
				"$ stackit project list"),
			examples.NewExample(
				`List all STACKIT projects that are children of a specific parent`,
				"$ stackit project list --parent-id xxx"),
			examples.NewExample(
				`List all STACKIT projects that match the given project IDs, located under the same parent resource`,
				"$ stackit project list --project-id-like xxx,yyy,zzz"),
			examples.NewExample(
				`List all STACKIT projects that a certain user is a member of`,
				"$ stackit project list --member example@email.com"),
		),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			model, err := parseInput(params.Printer, cmd, args)
			if err != nil {
				return err
			}

			// Configure API client
			authorizationClient, err := authorizationclient.ConfigureClient(params.Printer, params.CliVersion)
			if err != nil {
				return err
			}

			resourcemanagerClient, err := resourcemanagerclient.ConfigureClient(params.Printer, params.CliVersion)
			if err != nil {
				return err
			}

			// Fetch projects
			projects, err := fetchProjects(ctx, model, authorizationClient, resourcemanagerClient)
			if err != nil {
				return err
			}
			if len(projects) == 0 {
				params.Printer.Info("No projects found matching the criteria\n")
				return nil
			}

			return outputResult(params.Printer, model.OutputFormat, projects)
		},
	}
	configureFlags(cmd)
	return cmd
}

func configureFlags(cmd *cobra.Command) {
	cmd.Flags().String(organizationNameFlag, "", "Filter by organization name")
	cmd.Flags().String(folderNameFlag, "", "Filter by folder name")
	cmd.Flags().Var(flags.UUIDSliceFlag(), folderIdFlag, "Filter by folder identifier. Multiple project IDs can be provided, but they need to belong to the same parent resource")
	cmd.Flags().Var(flags.UUIDSliceFlag(), organizationIdFlag, "Filter by organization identifier. Multiple project IDs can be provided, but they need to belong to the same parent resource")
	cmd.Flags().String(memberFlag, "", "Filter by member. The list of projects of which the member is part of will be shown")
	cmd.Flags().String(creationTimeAfterFlag, "", "Filter by creation timestamp, in a date-time with the RFC3339 layout format, e.g. 2023-01-01T00:00:00Z. The list of projects that were created after the given timestamp will be shown")
}

func parseInput(p *print.Printer, cmd *cobra.Command, _ []string) (*inputModel, error) {
	globalFlags := globalflags.Parse(p, cmd)

	creationTimeAfter, err := flags.FlagToDateTimePointer(p, cmd, creationTimeAfterFlag, creationTimeAfterFormat)
	if err != nil {
		return nil, &errors.FlagValidationError{
			Flag:    creationTimeAfterFlag,
			Details: err.Error(),
		}
	}

	model := inputModel{
		GlobalFlagModel:   globalFlags,
		OrganizationId:    flags.FlagToStringSliceValue(p, cmd, organizationIdFlag),
		OrganizationName:  flags.FlagToStringPointer(p, cmd, organizationNameFlag),
		FolderId:          flags.FlagToStringSliceValue(p, cmd, folderIdFlag),
		FolderName:        flags.FlagToStringPointer(p, cmd, folderNameFlag),
		Member:            flags.FlagToStringPointer(p, cmd, memberFlag),
		CreationTimeAfter: creationTimeAfter,
	}

	p.DebugInputModel(model)
	return &model, nil
}

func getUserMemberships(ctx context.Context, model *inputModel, apiClient *authorization.APIClient) ([]authorization.UserMembership, error) {
	var member string
	if model.Member != nil {
		member = *model.Member
	} else {
		var err error
		member, err = auth.GetAuthEmail()
		if err != nil {
			return nil, fmt.Errorf("get email of authenticated user: %w", err)
		}
	}

	resp, err := apiClient.ListUserMemberships(ctx, member).Execute()
	if err != nil {
		return nil, err
	}
	return resp.GetItems(), nil
}

type organization struct {
	Name string
	Id   string
}

func getProjectDetails(ctx context.Context, id string, creationTimeAfter *time.Time, client *resourcemanager.APIClient) (*resourcemanager.Project, organization, []resourcemanager.ParentListInner, error) {
	resp, err := client.GetProject(ctx, id).IncludeParents(true).Execute()
	if err != nil {
		return nil, organization{}, nil, err
	}
	if creationTimeAfter != nil {
		if !resp.CreationTime.After(*creationTimeAfter) {
			return nil, organization{}, nil, nil
		}
	}

	org, err := getOrganizationDetailsFromParents(ctx, resp.GetParents(), client)
	if err != nil {
		return nil, org, nil, err
	}

	return &resourcemanager.Project{
		ContainerId:    resp.ContainerId,
		CreationTime:   resp.CreationTime,
		Labels:         resp.Labels,
		LifecycleState: resp.LifecycleState,
		Name:           resp.Name,
		Parent:         resp.Parent,
		ProjectId:      resp.ProjectId,
		UpdateTime:     resp.UpdateTime,
	}, org, resp.GetParents(), nil
}

func getOrganizationDetailsFromParents(ctx context.Context, parents []resourcemanager.ParentListInner, client *resourcemanager.APIClient) (organization, error) {
	for _, parent := range parents {
		if parent.GetType() == "ORGANIZATION" {
			orgResp, err := client.GetOrganizationExecute(ctx, parent.GetContainerId())
			if err != nil {
				return organization{}, err
			}
			return organization{
				Name: orgResp.GetName(),
				Id:   orgResp.GetOrganizationId(),
			}, nil
		}
	}
	return organization{}, stderrors.New("organization not found")
}

func getFolderOrganization(ctx context.Context, folderId string, client *resourcemanager.APIClient) (organization, error) {
	resp, err := client.GetFolderDetails(ctx, folderId).IncludeParents(true).Execute()
	if err != nil {
		return organization{}, err
	}

	return getOrganizationDetailsFromParents(ctx, resp.GetParents(), client)
}

func getProjectsFromParent(ctx context.Context, parentId string, client *resourcemanager.APIClient) ([]resourcemanager.Project, error) {
	resp, err := client.ListProjects(ctx).ContainerParentId(parentId).Execute()
	if err != nil {
		return nil, err
	}
	return resp.GetItems(), nil
}

type folderMap struct {
	mu    sync.Mutex
	c     *resourcemanager.APIClient
	cache map[string][]string
}

func (f *folderMap) GetProjectFolderPath(ctx context.Context, p *resourcemanager.Project) ([]string, error) {
	parent := p.GetParent()
	if parent.GetType() != "FOLDER" {
		return []string{}, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	folderpath, ok := f.cache[parent.GetContainerId()]
	if !ok {
		var err error
		folderpath, err = f.getProjectFolderPath(ctx, parent.GetContainerId())
		if err != nil {
			return nil, err
		}
		f.cache[parent.GetContainerId()] = folderpath
	}
	return folderpath, nil
}

func (f *folderMap) getProjectFolderPath(ctx context.Context, parentId string) ([]string, error) {
	resp, err := f.c.GetFolderDetails(ctx, parentId).IncludeParents(true).Execute()
	if err != nil {
		return nil, err
	}
	folderpath := []string{resp.GetName()}
	for _, parent := range resp.GetParents() {
		// TODO: check if this is always returned sorted
		if parent.GetType() != "FOLDER" {
			break
		}
		folderpath = append([]string{parent.GetName()}, folderpath...)
	}
	return folderpath, nil
}

type projectInfo struct {
	resourcemanager.Project
	FolderPath folderPath
}

type folderPath []string

func (f folderPath) String() string {
	return path.Join(f...)
}

type projectInOrgs struct {
	mu    sync.Mutex
	state map[string]map[string]projectInfo
	fmap  folderMap

	folderFilters      []func(*resourcemanager.GetFolderDetailsResponse) bool
	organizationFilter []func(organization) bool

	model                 *inputModel
	resourcemanagerClient *resourcemanager.APIClient
}

func (o *projectInOrgs) processProject(ctx context.Context, item *authorization.UserMembership) error {
	proj, org, parents, err := getProjectDetails(ctx, *item.ResourceId, o.model.CreationTimeAfter, o.resourcemanagerClient)
	if err != nil {
		return err
	}
	if proj == nil {
		return nil
	}

	for _, filter := range o.organizationFilter {
		if filter(org) {
			return nil
		}
	}
	if len(o.folderFilters) != 0 {
		p, ok := proj.GetParentOk()
		// folder filters are set, but no parent
		if !ok {
			return nil
		}
		// folder filters are set, but the parent is not a folder, so sort out every time
		if p.GetType() != "FOLDER" {
			return nil
		}

		var found bool
		for _, parent := range parents {
			if parent.GetType() != "FOLDER" {
				continue
			}
			details, err := o.resourcemanagerClient.GetFolderDetails(ctx, parent.GetId()).Execute()
			if err != nil {
				return err
			}
			for _, filter := range o.folderFilters {
				if !filter(details) {
					found = true
				}
			}
		}
		if !found {
			return nil
		}
	}

	folderPath, err := o.fmap.GetProjectFolderPath(ctx, proj)
	if err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	orgMap, ok := o.state[org.Name]
	if !ok {
		o.state[org.Name] = map[string]projectInfo{}
		orgMap = o.state[org.Name]
	}
	if _, ok := orgMap[proj.GetProjectId()]; !ok {
		orgMap[proj.GetProjectId()] = projectInfo{
			Project:    *proj,
			FolderPath: folderPath,
		}
	}
	return nil
}

func (o *projectInOrgs) processFolder(ctx context.Context, item *authorization.UserMembership) error {
	if len(o.folderFilters) != 0 {
		details, err := o.resourcemanagerClient.GetFolderDetails(ctx, item.GetResourceId()).IncludeParents(true).Execute()
		if err != nil {
			return err
		}
		// Sort out folder
		allFolderDetails := make([]*resourcemanager.GetFolderDetailsResponse, 0, 1+len(details.GetParents()))
		allFolderDetails = append(allFolderDetails, details)
		for _, parent := range details.GetParents() {
			if parent.GetType() != "FOLDER" {
				continue
			}
			details, err := o.resourcemanagerClient.GetFolderDetails(ctx, parent.GetId()).Execute()
			if err != nil {
				return err
			}
			allFolderDetails = append(allFolderDetails, details)
		}
		var found bool
		for _, d := range allFolderDetails {
			for _, filter := range o.folderFilters {
				if !filter(d) {
					found = true
				}
			}
		}
		if !found {
			return nil
		}
	}

	org, err := getFolderOrganization(ctx, item.GetResourceId(), o.resourcemanagerClient)
	if err != nil {
		return err
	}

	// Sort out org
	for _, filter := range o.organizationFilter {
		if filter(org) {
			return nil
		}
	}

	projects, err := getProjectsFromParent(ctx, item.GetResourceId(), o.resourcemanagerClient)
	if err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	orgMap, ok := o.state[org.Name]
	if !ok {
		o.state[org.Name] = map[string]projectInfo{}
		orgMap = o.state[org.Name]
	}
	for _, proj := range projects {
		folderPath, err := o.fmap.GetProjectFolderPath(ctx, &proj)
		if err != nil {
			return err
		}
		if _, ok := orgMap[proj.GetProjectId()]; !ok {
			orgMap[proj.GetProjectId()] = projectInfo{
				Project:    proj,
				FolderPath: folderPath,
			}
		}
	}
	return nil
}

// AsSortedSlice returns the projectInfos sorted by folderPath
func (o *projectInOrgs) AsSortedSlice() map[string][]projectInfo {
	infoMap := make(map[string][]projectInfo, len(o.state))
	for org, m := range o.state {
		infos := slices.Collect(maps.Values(m))
		slices.SortFunc(infos, func(a, b projectInfo) int {
			return cmp.Compare(a.FolderPath.String(), b.FolderPath.String())
		})
		infoMap[org] = infos
	}
	return infoMap
}

func organizationFiltersFromModel(model *inputModel) []func(organization) bool {
	filters := []func(organization) bool{}
	if len(model.OrganizationId) != 0 {
		filters = append(filters, func(o organization) bool {
			return !slices.Contains(model.OrganizationId, o.Id)
		})
	}
	if model.OrganizationName != nil {
		filters = append(filters, func(o organization) bool {
			return o.Name != *model.OrganizationName
		})
	}
	return filters
}

func folderFiltersFromModel(model *inputModel) []func(*resourcemanager.GetFolderDetailsResponse) bool {
	filters := []func(*resourcemanager.GetFolderDetailsResponse) bool{}
	if len(model.FolderId) != 0 {
		filters = append(filters, func(r *resourcemanager.GetFolderDetailsResponse) bool {
			return !slices.Contains(model.FolderId, r.GetFolderId())
		})
	}
	if model.FolderName != nil {
		filters = append(filters, func(r *resourcemanager.GetFolderDetailsResponse) bool {
			return r.GetName() != *model.FolderName
		})
	}
	return filters
}

func fetchProjects(ctx context.Context, model *inputModel, authorizationClient *authorization.APIClient, resourcemanagerClient *resourcemanager.APIClient) (map[string][]projectInfo, error) {
	projectOrgMap := projectInOrgs{
		resourcemanagerClient: resourcemanagerClient,
		state:                 map[string]map[string]projectInfo{},
		fmap: folderMap{
			cache: map[string][]string{},
			c:     resourcemanagerClient,
		},
		model:              model,
		folderFilters:      folderFiltersFromModel(model),
		organizationFilter: organizationFiltersFromModel(model),
	}

	memberships, err := getUserMemberships(ctx, model, authorizationClient)
	if err != nil {
		return nil, err
	}

	g := new(errgroup.Group)
	for _, item := range memberships {
		if item.ResourceId == nil {
			continue
		}
		switch *item.ResourceType {
		case "project":
			g.Go(func() error {
				return projectOrgMap.processProject(ctx, &item)
			})
		case "folder":
			g.Go(func() error {
				return projectOrgMap.processFolder(ctx, &item)
			})
		}
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	return projectOrgMap.AsSortedSlice(), nil
}

func outputResult(p *print.Printer, outputFormat string, projectInfos map[string][]projectInfo) error {
	return p.OutputResult(outputFormat, projectInfos, func() error {
		table := tables.NewTable()
		table.SetHeader("ID", "ORGANIZATION", "FOLDER", "NAME", "STATE", "PARENT ID")
		for org, projects := range projectInfos {
			for _, p := range projects {
				var parentId *string
				if p.Parent != nil {
					parentId = p.Parent.Id
				}
				table.AddRow(
					utils.PtrString(p.ProjectId),
					org,
					p.FolderPath,
					utils.PtrString(p.Name),
					utils.PtrString(p.LifecycleState),
					utils.PtrString(parentId),
				)
			}
		}

		err := table.Display(p)
		if err != nil {
			return fmt.Errorf("render table: %w", err)
		}

		return nil
	})
}
