package render

// Additional hand-written Vulkan ABI definitions for the graphics pipeline the
// Vulkan renderer uses (shader modules, descriptor sets, render pass and the
// pipeline itself). Like vk_types.go these mirror vulkan_core.h byte for byte.

// VkStructureType values for the pipeline path.
const (
	stImageViewCreateInfo        = 15
	stShaderModuleCreateInfo     = 16
	stPipelineShaderStageInfo    = 18
	stPipelineVertexInput        = 19
	stPipelineInputAssembly      = 20
	stPipelineViewportState      = 22
	stPipelineRasterization      = 23
	stPipelineMultisample        = 24
	stPipelineColorBlend         = 26
	stPipelineDynamicState       = 27
	stGraphicsPipelineCreateInfo = 28
	stPipelineLayoutCreateInfo   = 30
	stSamplerCreateInfo          = 31
	stDescriptorSetLayoutCreate  = 32
	stDescriptorPoolCreateInfo   = 33
	stDescriptorSetAllocateInfo  = 34
	stWriteDescriptorSet         = 35
	stFramebufferCreateInfo      = 37
	stRenderPassCreateInfo       = 38
	stRenderPassBeginInfo        = 43
)

// VkShaderStageFlagBits
const (
	shaderStageVertex   = 0x1
	shaderStageFragment = 0x10
)

// VkPipelineStageFlagBits / VkAccessFlagBits used by the pipeline barriers.
const (
	stageFragmentShader       = 0x10
	stageColorAttachmentOut   = 0x400
	accessShaderRead          = 0x20
	accessColorAttachmentWrit = 0x100
)

// VkSubpassExternal is VK_SUBPASS_EXTERNAL.
const vkSubpassExternal = ^uint32(0)

// VkImageUsageFlagBits (image views / render targets)
const usageColorAttachment = 0x10

// VkImageLayout additions
const layoutShaderReadOnly = 5

// VkDescriptorType / VkDescriptorPoolCreateFlagBits
const (
	descTypeCombinedImageSampler = 1
	descPoolFreeDescriptorSet    = 0x1
)

// Pipeline enums
const (
	topologyTriangleStrip = 4
	polygonModeFill       = 0
	cullModeNone          = 0
	frontFaceCCW          = 1
	sampleCount1          = 0x1
	blendZero             = 0
	blendOne              = 1
	blendSrcAlpha         = 6
	blendOneMinusSrcAlpha = 7
	blendOpAdd            = 0
	colorComponentRGBA    = 0xF
	dynamicStateViewport  = 0
	dynamicStateScissor   = 1
	loadOpClear           = 1
	storeOpStore          = 0
	imageViewType2D       = 1
	filterNearest         = 0
	filterLinear          = 1
	mipmapModeNearest     = 0
	addressModeClampEdge  = 2
	borderColorTransBlack = 2
	bindPointGraphics     = 0
)

type vkComponentMapping struct{ R, G, B, A uint32 }

type vkImageViewCreateInfo struct {
	SType            uint32
	PNext            uintptr
	Flags            uint32
	Image            uintptr
	ViewType         uint32
	Format           uint32
	Components       vkComponentMapping
	SubresourceRange vkImageSubresourceRange
}

type vkShaderModuleCreateInfo struct {
	SType    uint32
	PNext    uintptr
	Flags    uint32
	CodeSize uintptr
	PCode    uintptr
}

type vkDescriptorSetLayoutBinding struct {
	Binding            uint32
	DescriptorType     uint32
	DescriptorCount    uint32
	StageFlags         uint32
	PImmutableSamplers uintptr
}

type vkDescriptorSetLayoutCreateInfo struct {
	SType        uint32
	PNext        uintptr
	Flags        uint32
	BindingCount uint32
	PBindings    uintptr
}

type vkPushConstantRange struct {
	StageFlags uint32
	Offset     uint32
	Size       uint32
}

type vkPipelineLayoutCreateInfo struct {
	SType                  uint32
	PNext                  uintptr
	Flags                  uint32
	SetLayoutCount         uint32
	PSetLayouts            uintptr
	PushConstantRangeCount uint32
	PPushConstantRanges    uintptr
}

type vkPipelineShaderStageCreateInfo struct {
	SType               uint32
	PNext               uintptr
	Flags               uint32
	Stage               uint32
	Module              uintptr
	PName               uintptr
	PSpecializationInfo uintptr
}

type vkPipelineVertexInputStateCreateInfo struct {
	SType                           uint32
	PNext                           uintptr
	Flags                           uint32
	VertexBindingDescriptionCount   uint32
	PVertexBindingDescriptions      uintptr
	VertexAttributeDescriptionCount uint32
	PVertexAttributeDescriptions    uintptr
}

type vkPipelineInputAssemblyStateCreateInfo struct {
	SType                  uint32
	PNext                  uintptr
	Flags                  uint32
	Topology               uint32
	PrimitiveRestartEnable uint32
}

type vkViewport struct {
	X, Y, Width, Height float32
	MinDepth, MaxDepth  float32
}

type vkOffset2D struct{ X, Y int32 }
type vkExtent2D struct{ Width, Height uint32 }
type vkRect2D struct {
	Offset vkOffset2D
	Extent vkExtent2D
}

type vkPipelineViewportStateCreateInfo struct {
	SType         uint32
	PNext         uintptr
	Flags         uint32
	ViewportCount uint32
	PViewports    uintptr
	ScissorCount  uint32
	PScissors     uintptr
}

type vkPipelineRasterizationStateCreateInfo struct {
	SType                   uint32
	PNext                   uintptr
	Flags                   uint32
	DepthClampEnable        uint32
	RasterizerDiscardEnable uint32
	PolygonMode             uint32
	CullMode                uint32
	FrontFace               uint32
	DepthBiasEnable         uint32
	_                       uint32 // C padding before the float block
	DepthBiasConstantFactor float32
	DepthBiasClamp          float32
	DepthBiasSlopeFactor    float32
	LineWidth               float32
}

type vkPipelineMultisampleStateCreateInfo struct {
	SType                 uint32
	PNext                 uintptr
	Flags                 uint32
	RasterizationSamples  uint32
	SampleShadingEnable   uint32
	MinSampleShading      float32
	PSampleMask           uintptr
	AlphaToCoverageEnable uint32
	AlphaToOneEnable      uint32
}

type vkPipelineColorBlendAttachmentState struct {
	BlendEnable         uint32
	SrcColorBlendFactor uint32
	DstColorBlendFactor uint32
	ColorBlendOp        uint32
	SrcAlphaBlendFactor uint32
	DstAlphaBlendFactor uint32
	AlphaBlendOp        uint32
	ColorWriteMask      uint32
}

type vkPipelineColorBlendStateCreateInfo struct {
	SType           uint32
	PNext           uintptr
	Flags           uint32
	LogicOpEnable   uint32
	LogicOp         uint32
	AttachmentCount uint32
	PAttachments    uintptr
	BlendConstants  [4]float32
}

type vkPipelineDynamicStateCreateInfo struct {
	SType             uint32
	PNext             uintptr
	Flags             uint32
	DynamicStateCount uint32
	PDynamicStates    uintptr
}

type vkGraphicsPipelineCreateInfo struct {
	SType               uint32
	PNext               uintptr
	Flags               uint32
	StageCount          uint32
	PStages             uintptr
	PVertexInputState   uintptr
	PInputAssemblyState uintptr
	PTessellationState  uintptr
	PViewportState      uintptr
	PRasterizationState uintptr
	PMultisampleState   uintptr
	PDepthStencilState  uintptr
	PColorBlendState    uintptr
	PDynamicState       uintptr
	Layout              uintptr
	RenderPass          uintptr
	Subpass             uint32
	_                   uint32
	BasePipelineHandle  uintptr
	BasePipelineIndex   int32
}

type vkAttachmentDescription struct {
	Flags          uint32
	Format         uint32
	Samples        uint32
	LoadOp         uint32
	StoreOp        uint32
	StencilLoadOp  uint32
	StencilStoreOp uint32
	InitialLayout  uint32
	FinalLayout    uint32
}

type vkAttachmentReference struct {
	Attachment uint32
	Layout     uint32
}

type vkSubpassDescription struct {
	Flags                   uint32
	PipelineBindPoint       uint32
	InputAttachmentCount    uint32
	PInputAttachments       uintptr
	ColorAttachmentCount    uint32
	PColorAttachments       uintptr
	PResolveAttachments     uintptr
	PDepthStencilAttachment uintptr
	PreserveAttachmentCount uint32
	PPreserveAttachments    uintptr
}

type vkRenderPassCreateInfo struct {
	SType           uint32
	PNext           uintptr
	Flags           uint32
	AttachmentCount uint32
	PAttachments    uintptr
	SubpassCount    uint32
	PSubpasses      uintptr
	DependencyCount uint32
	PDependencies   uintptr
}

type vkSubpassDependency struct {
	SrcSubpass      uint32
	DstSubpass      uint32
	SrcStageMask    uint32
	DstStageMask    uint32
	SrcAccessMask   uint32
	DstAccessMask   uint32
	DependencyFlags uint32
}

type vkRenderPassBeginInfo struct {
	SType           uint32
	PNext           uintptr
	RenderPass      uintptr
	Framebuffer     uintptr
	RenderArea      vkRect2D
	ClearValueCount uint32
	PClearValues    uintptr
}

type vkFramebufferCreateInfo struct {
	SType           uint32
	PNext           uintptr
	Flags           uint32
	RenderPass      uintptr
	AttachmentCount uint32
	PAttachments    uintptr
	Width           uint32
	Height          uint32
	Layers          uint32
}

type vkSamplerCreateInfo struct {
	SType                   uint32
	PNext                   uintptr
	Flags                   uint32
	MagFilter               uint32
	MinFilter               uint32
	MipmapMode              uint32
	AddressModeU            uint32
	AddressModeV            uint32
	AddressModeW            uint32
	MipLodBias              float32
	AnisotropyEnable        uint32
	MaxAnisotropy           float32
	CompareEnable           uint32
	CompareOp               uint32
	MinLod                  float32
	MaxLod                  float32
	BorderColor             uint32
	UnnormalizedCoordinates uint32
}

type vkDescriptorPoolSize struct {
	Type            uint32
	DescriptorCount uint32
}

type vkDescriptorPoolCreateInfo struct {
	SType         uint32
	PNext         uintptr
	Flags         uint32
	MaxSets       uint32
	PoolSizeCount uint32
	PPoolSizes    uintptr
}

type vkDescriptorSetAllocateInfo struct {
	SType              uint32
	PNext              uintptr
	DescriptorPool     uintptr
	DescriptorSetCount uint32
	PSetLayouts        uintptr
}

type vkDescriptorImageInfo struct {
	Sampler     uintptr
	ImageView   uintptr
	ImageLayout uint32
}

type vkWriteDescriptorSet struct {
	SType            uint32
	PNext            uintptr
	DstSet           uintptr
	DstBinding       uint32
	DstArrayElement  uint32
	DescriptorCount  uint32
	DescriptorType   uint32
	PImageInfo       uintptr
	PBufferInfo      uintptr
	PTexelBufferView uintptr
}

type vkCopyBuffer struct {
	SrcOffset uint64
	DstOffset uint64
	Size      uint64
}
