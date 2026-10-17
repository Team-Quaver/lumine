package render

// Hand-written Vulkan ABI definitions for the small subset Lumine's renderer
// needs. The layout of every struct below matches vulkan_core.h byte for byte;
// the renderer talks to libvulkan.so.1 through PureGo (dlopen/dlsym), so there
// is no CGO and no libvulkan headers at build time.

const (
	vkSuccess        = 0
	vkNotReady       = 1
	vkTimeout        = 2
	vkIncomplete     = 5
	vkErrorOutOfHost = -1
	vkErrorInitFail  = -3
	vkErrorDevLost   = -4
)

// VkStructureType values used here.
const (
	stApplicationInfo          = 0
	stInstanceCreateInfo       = 1
	stDeviceQueueCreateInfo    = 2
	stDeviceCreateInfo         = 3
	stSubmitInfo               = 4
	stMemoryAllocateInfo       = 5
	stFenceCreateInfo          = 8
	stBufferCreateInfo         = 12
	stImageCreateInfo          = 14
	stCommandPoolCreateInfo    = 39
	stCommandBufferAllocInfo   = 40
	stCommandBufferBeginInfo   = 42
	stBufferMemoryBarrier      = 44
	stImageMemoryBarrier       = 45
	stMemoryBarrier            = 46
	stImageFormatProperties2   = 1000059006
	stPhysicalDeviceProperties = 1000059000
)

// VkFormat
const (
	fmtB8G8R8A8UNorm = 44
	fmtB8G8R8A8SRGB  = 50
)

// VkImageTiling / VkImageType / VkSharingMode / VkImageLayout
const (
	tilingOptimal      = 0
	tilingLinear       = 1
	imageType2D        = 1
	sharingExclusive   = 0
	layoutUndefined    = 0
	layoutGeneral      = 1
	layoutTransferSrc  = 6
	layoutTransferDst  = 7
	layoutPreinit      = 8
	cmdBufferLevelPrim = 0
	cmdPoolResetBuf    = 0x2
	fenceSignaled      = 0x1
)

// VkImageUsageFlagBits
const (
	usageTransferSrc = 0x1
	usageTransferDst = 0x2
	usageSampled     = 0x4
)

// VkImageAspectFlagBits
const (
	aspectColor = 0x1
)

// VkMemoryPropertyFlagBits
const (
	memDeviceLocal  = 0x1
	memHostVisible  = 0x2
	memHostCoherent = 0x4
)

// VkPipelineStageFlagBits / VkAccessFlagBits
const (
	stageTopOfPipe = 0x1
	stageTransfer  = 0x1000
	stageBottom    = 0x2000
	accessXferRead = 0x800
	accessXferWrit = 0x1000
	accessHostRead = 0x2000
)

// VkQueueFlagBits
const (
	queueGraphics = 0x1
	queueCompute  = 0x2
	queueTransfer = 0x4
)

// VkImageFormatProperties: only used to validate blit support.
const (
	imageFormatFeatureBlitSrc = 0x400
	imageFormatFeatureBlitDst = 0x800
)

// VkImageFormatProperties is passed as a plain byte blob: maxExtent (3x int32),
// maxMipLevels, maxArrayLayers, sampleCounts, maxResourceSize.
type vkImageFormatProperties [8]uint32

type vkExtent3D struct{ Width, Height, Depth int32 }
type vkOffset3D struct{ X, Y, Z int32 }

type vkApplicationInfo struct {
	SType              uint32
	PNext              uintptr
	PApplicationName   uintptr
	ApplicationVersion uint32
	PEngineName        uintptr
	EngineVersion      uint32
	ApiVersion         uint32
}

type vkInstanceCreateInfo struct {
	SType                   uint32
	PNext                   uintptr
	Flags                   uint32
	PApplicationInfo        uintptr
	EnabledLayerCount       uint32
	PpEnabledLayerNames     uintptr
	EnabledExtensionCount   uint32
	PpEnabledExtensionNames uintptr
}

type vkDeviceQueueCreateInfo struct {
	SType            uint32
	PNext            uintptr
	Flags            uint32
	QueueFamilyIndex uint32
	QueueCount       uint32
	PQueuePriorities uintptr
}

type vkDeviceCreateInfo struct {
	SType                   uint32
	PNext                   uintptr
	Flags                   uint32
	QueueCreateInfoCount    uint32
	PQueueCreateInfos       uintptr
	EnabledLayerCount       uint32
	PpEnabledLayerNames     uintptr
	EnabledExtensionCount   uint32
	PpEnabledExtensionNames uintptr
	PEnabledFeatures        uintptr
}

type vkMemoryAllocateInfo struct {
	SType           uint32
	PNext           uintptr
	AllocationSize  uint64
	MemoryTypeIndex uint32
}

// physicalDeviceMemoryProperties is read from a raw buffer: typeCount at 0,
// memoryTypes at 4, heapCount at 260, heaps at 264.
type vkMemoryType struct {
	PropertyFlags uint32
	HeapIndex     uint32
}

type vkMemoryHeap struct {
	Size  uint64
	Flags uint32
}

type vkImageCreateInfo struct {
	SType                 uint32
	PNext                 uintptr
	Flags                 uint32
	ImageType             uint32
	Format                uint32
	Extent                vkExtent3D
	MipLevels             uint32
	ArrayLayers           uint32
	Samples               uint32
	Tiling                uint32
	Usage                 uint32
	SharingMode           uint32
	QueueFamilyIndexCount uint32
	PQueueFamilyIndices   uintptr
	InitialLayout         uint32
}

type vkBufferCreateInfo struct {
	SType                 uint32
	PNext                 uintptr
	Flags                 uint32
	Size                  uint64
	Usage                 uint32
	SharingMode           uint32
	QueueFamilyIndexCount uint32
	PQueueFamilyIndices   uintptr
}

type vkMemoryRequirements struct {
	Size           uint64
	Alignment      uint64
	MemoryTypeBits uint32
}

type vkImageSubresourceRange struct {
	AspectMask     uint32
	BaseMipLevel   uint32
	LevelCount     uint32
	BaseArrayLayer uint32
	LayerCount     uint32
}

type vkImageSubresourceLayers struct {
	AspectMask     uint32
	MipLevel       uint32
	BaseArrayLayer uint32
	LayerCount     uint32
}

type vkImageMemoryBarrier struct {
	SType               uint32
	PNext               uintptr
	SrcAccessMask       uint32
	DstAccessMask       uint32
	OldLayout           uint32
	NewLayout           uint32
	SrcQueueFamilyIndex uint32
	DstQueueFamilyIndex uint32
	Image               uintptr
	SubresourceRange    vkImageSubresourceRange
}

type vkImageBlit struct {
	SrcSubresource vkImageSubresourceLayers
	SrcOffsets     [2]vkOffset3D
	DstSubresource vkImageSubresourceLayers
	DstOffsets     [2]vkOffset3D
}

type vkImageCopy struct {
	SrcSubresource vkImageSubresourceLayers
	SrcOffset      vkOffset3D
	DstSubresource vkImageSubresourceLayers
	DstOffset      vkOffset3D
	Extent         vkExtent3D
}

type vkBufferImageCopy struct {
	BufferOffset      uint64
	BufferRowLength   uint32
	BufferImageHeight uint32
	ImageSubresource  vkImageSubresourceLayers
	ImageOffset       vkOffset3D
	ImageExtent       vkExtent3D
}

type vkClearColorValue struct {
	Float32 [4]float32
	Uint32  [4]uint32
	Int32   [4]int32
}

type vkCommandPoolCreateInfo struct {
	SType            uint32
	PNext            uintptr
	Flags            uint32
	QueueFamilyIndex uint32
}

type vkCommandBufferAllocateInfo struct {
	SType              uint32
	PNext              uintptr
	CommandPool        uintptr
	Level              uint32
	CommandBufferCount uint32
}

type vkCommandBufferBeginInfo struct {
	SType            uint32
	PNext            uintptr
	Flags            uint32
	PInheritanceInfo uintptr
}

type vkSubmitInfo struct {
	SType                uint32
	PNext                uintptr
	WaitSemaphoreCount   uint32
	PWaitSemaphores      uintptr
	PWaitDstStageMask    uintptr
	CommandBufferCount   uint32
	PCommandBuffers      uintptr
	SignalSemaphoreCount uint32
	PSignalSemaphores    uintptr
}

type vkFenceCreateInfo struct {
	SType uint32
	PNext uintptr
	Flags uint32
}
