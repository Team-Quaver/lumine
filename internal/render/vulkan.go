package render

import (
	_ "embed"
	"encoding/binary"
	"fmt"
	"sync"
	"unsafe"

	"github.com/Team-Quaver/lumine/internal/shm"
)

// The two shaders are compiled from internal/render/shaders/*.vert|frag with
// glslangValidator --target-env vulkan1.0 and embedded so the compositor never
// needs a shader compiler, headers or CGO at runtime.
//
//go:embed shaders/quad.vert.spv
var quadVertSPV []byte

//go:embed shaders/quad.frag.spv
var quadFragSPV []byte

// Vulkan is the GPU renderer. It rasterises the frame on the device with a
// single textured-quad pipeline and then copies the result into the
// CPU-visible Framebuffer, which is what every Lumine backend presents
// (wl_shm buffer when nested, DRM dumb buffer on bare metal).
//
// The renderer deliberately targets the lowest common denominator of Vulkan
// 1.0: one offscreen image, one render pass, no swapchain, no semaphores and
// no vertex buffers.
type Vulkan struct {
	api   *vkAPI
	theme Theme
	cs    colorState

	mu sync.Mutex

	inst   uintptr
	phys   uintptr
	dev    uintptr
	queue  uintptr
	qfam   uint32
	memT   vkPhysicalDeviceMemoryProperties
	device string

	cmdPool  uintptr
	cmdBuf   uintptr
	fence    uintptr
	sampler  uintptr
	pass     uintptr
	setLay   uintptr
	pipeLay  uintptr
	pipeline uintptr
	descPool uintptr

	// output-sized resources
	fw, fh   int
	outImage uintptr
	outMem   uintptr
	outView  uintptr
	framebuf uintptr
	readback uintptr
	rbMem    uintptr
	rbPtr    unsafe.Pointer
	rbSize   uint64

	// host-visible staging area reused for every texture upload
	staging     uintptr
	stgMem      uintptr
	stgPtr      unsafe.Pointer
	stagingSize uint64

	white   *vkTexture
	texture map[*shm.Image]*vkTexture

	closed bool
}

type vkTexture struct {
	image uintptr
	mem   uintptr
	view  uintptr
	w, h  int
	// layout is the image's current layout, tracked across frames because the
	// texture is re-uploaded (and re-transitioned) on every Draw.
	layout uint32
	white  bool
}

type vkPhysicalDeviceMemoryProperties struct {
	MemoryTypeCount uint32
	MemoryTypes     [32]vkMemoryType
	MemoryHeapCount uint32
	MemoryHeaps     [16]vkMemoryHeap
}

const vkAPIVersion10 = 1 << 22 // VK_MAKE_API_VERSION(0, 1, 0, 0)

// VulkanAvailable reports whether a usable Vulkan device is present. It is
// cheap enough for a startup probe and never panics: a missing loader or a
// device without the required format support simply returns false.
func VulkanAvailable() (bool, string) {
	v, err := NewVulkan(DefaultTheme(), 0, 0)
	if err != nil {
		return false, err.Error()
	}
	name := v.device
	v.Close()
	return true, name
}

// NewVulkan brings up the GPU renderer for one output. It returns an error
// (and no renderer) when no suitable device exists, so callers can fall back
// to CPU.
func NewVulkan(theme Theme, w, h int) (*Vulkan, error) {
	api, err := loadVulkan()
	if err != nil {
		return nil, err
	}
	v := &Vulkan{api: api, theme: theme, texture: map[*shm.Image]*vkTexture{}}
	if err := v.initDevice(); err != nil {
		v.Close()
		return nil, err
	}
	if err := v.initPipeline(); err != nil {
		v.Close()
		return nil, err
	}
	if w > 0 && h > 0 {
		if err := v.ensureTargets(w, h); err != nil {
			v.Close()
			return nil, err
		}
	}
	return v, nil
}

// Name implements the renderer interface.
func (v *Vulkan) Name() string { return "vulkan" }

// SetColorSpace records the negotiated output color state. The transfer
// function is applied to the composited frame by Draw (see color.go).
func (v *Vulkan) SetColorSpace(enabled bool, tf, primaries string, maxLuma uint32) {
	v.mu.Lock()
	v.cs = colorState{Enabled: enabled, TF: tf, Primaries: primaries, MaxLuma: maxLuma}
	v.mu.Unlock()
}

func (v *Vulkan) initDevice() error {
	ai := vkApplicationInfo{
		SType:            stApplicationInfo,
		PApplicationName: uintptr(unsafe.Pointer(&appName[0])),
		PEngineName:      uintptr(unsafe.Pointer(&engineName[0])),
		ApiVersion:       vkAPIVersion10,
	}
	ci := vkInstanceCreateInfo{
		SType:            stInstanceCreateInfo,
		PApplicationInfo: uintptr(unsafe.Pointer(&ai)),
	}
	if rc := v.api.createInstance(&ci, nil, &v.inst); rc != vkSuccess {
		return fmt.Errorf("vulkan: vkCreateInstance failed (VkResult %d)", rc)
	}
	phys, name, err := v.api.pickDevice(v.inst)
	if err != nil {
		return err
	}
	v.phys, v.device = phys, name

	var qprops [8]vkQueueFamilyProperties
	n := uint32(len(qprops))
	v.api.getQueueFamilies(phys, &n, &qprops[0])
	if n > uint32(len(qprops)) {
		n = uint32(len(qprops))
	}
	fam := -1
	for i := uint32(0); i < n; i++ {
		if qprops[i].QueueFlags&queueGraphics != 0 {
			fam = int(i)
			break
		}
	}
	if fam < 0 {
		return fmt.Errorf("vulkan: %s has no graphics queue", name)
	}
	v.qfam = uint32(fam)

	prio := float32(1)
	qci := vkDeviceQueueCreateInfo{
		SType:            stDeviceQueueCreateInfo,
		QueueFamilyIndex: v.qfam,
		QueueCount:       1,
		PQueuePriorities: uintptr(unsafe.Pointer(&prio)),
	}
	dci := vkDeviceCreateInfo{
		SType:                stDeviceCreateInfo,
		QueueCreateInfoCount: 1,
		PQueueCreateInfos:    uintptr(unsafe.Pointer(&qci)),
	}
	if rc := v.api.createDevice(phys, &dci, nil, &v.dev); rc != vkSuccess {
		return fmt.Errorf("vulkan: vkCreateDevice failed (VkResult %d)", rc)
	}
	v.api.getDeviceQueue(v.dev, v.qfam, 0, &v.queue)
	v.api.getMemProps(phys, unsafe.Pointer(&v.memT))

	cp := vkCommandPoolCreateInfo{SType: stCommandPoolCreateInfo, Flags: cmdPoolResetBuf, QueueFamilyIndex: v.qfam}
	if rc := v.api.createCmdPool(v.dev, &cp, nil, &v.cmdPool); rc != vkSuccess {
		return fmt.Errorf("vulkan: vkCreateCommandPool failed (VkResult %d)", rc)
	}
	ai2 := vkCommandBufferAllocateInfo{
		SType: stCommandBufferAllocInfo, CommandPool: v.cmdPool,
		Level: cmdBufferLevelPrim, CommandBufferCount: 1,
	}
	if rc := v.api.allocCmdBuffers(v.dev, &ai2, &v.cmdBuf); rc != vkSuccess {
		return fmt.Errorf("vulkan: vkAllocateCommandBuffers failed (VkResult %d)", rc)
	}
	// The fence must start unsignaled: submitWait always calls vkQueueSubmit
	// before vkWaitForFences, and passing an already-signaled fence to
	// vkQueueSubmit is invalid — it would make the first wait return without
	// any GPU synchronisation and let the next frame reset the command buffer
	// while the previous one is still executing.
	fi := vkFenceCreateInfo{SType: stFenceCreateInfo}
	if rc := v.api.createFence(v.dev, &fi, nil, &v.fence); rc != vkSuccess {
		return fmt.Errorf("vulkan: vkCreateFence failed (VkResult %d)", rc)
	}
	return nil
}

var (
	appName    = []byte("lumine\x00")
	engineName = []byte("lumine-go\x00")
)

// pickDevice chooses the first physical device that can render into an
// offscreen B8G8R8A8_UNORM image we can also copy to host memory.
func (a *vkAPI) pickDevice(inst uintptr) (uintptr, string, error) {
	n := uint32(8)
	var devs [8]uintptr
	if rc := a.enumeratePhysical(inst, &n, &devs[0]); rc != vkSuccess {
		return 0, "", fmt.Errorf("vulkan: vkEnumeratePhysicalDevices failed (VkResult %d)", rc)
	}
	if n > uint32(len(devs)) {
		n = uint32(len(devs))
	}
	if n == 0 {
		return 0, "", fmt.Errorf("vulkan: no physical devices")
	}
	var rejected []string
	for i := uint32(0); i < n; i++ {
		name := a.deviceName(devs[i])
		var props vkImageFormatProperties
		rc := a.getImageFormat(devs[i], fmtB8G8R8A8UNorm, imageType2D, tilingOptimal,
			usageColorAttachment|usageTransferSrc|usageTransferDst, 0, &props)
		if rc != vkSuccess {
			rejected = append(rejected, name)
			continue
		}
		return devs[i], name, nil
	}
	return 0, "", fmt.Errorf("vulkan: no device supports offscreen rendering (%v)", rejected)
}

// vkPhysicalDevicePropertiesSize is the 64-bit size of
// VkPhysicalDeviceProperties. The driver fills the whole struct, so the
// staging buffer must never be smaller than this: a short buffer makes the
// driver write past the Go allocation and corrupt the heap.
const vkPhysicalDevicePropertiesSize = 824

// vkPhysicalDeviceNameOffset/size locate VkPhysicalDeviceProperties.deviceName.
const (
	vkPhysicalDeviceNameOffset = 20
	vkPhysicalDeviceNameSize   = 256
)

func (a *vkAPI) deviceName(dev uintptr) string {
	var props [vkPhysicalDevicePropertiesSize]byte
	a.getPhysProps(dev, unsafe.Pointer(&props[0]))
	b := props[vkPhysicalDeviceNameOffset : vkPhysicalDeviceNameOffset+vkPhysicalDeviceNameSize]
	end := 0
	for end < len(b) && b[end] != 0 {
		end++
	}
	if end == 0 {
		return "unknown"
	}
	return string(b[:end])
}

// ---- pipeline setup ----

func (v *Vulkan) initPipeline() error {
	vert, err := v.shaderModule(quadVertSPV)
	if err != nil {
		return err
	}
	defer v.api.destroyShaderModule(v.dev, vert, nil)
	frag, err := v.shaderModule(quadFragSPV)
	if err != nil {
		return err
	}
	defer v.api.destroyShaderModule(v.dev, frag, nil)

	// Render pass: one colour attachment, cleared at the start of every frame
	// and handed over as TRANSFER_SRC so it can be copied to host memory.
	attach := vkAttachmentDescription{
		Format: fmtB8G8R8A8UNorm, Samples: sampleCount1,
		LoadOp: loadOpClear, StoreOp: storeOpStore,
		StencilLoadOp: 2, StencilStoreOp: 1,
		InitialLayout: layoutUndefined, FinalLayout: layoutTransferSrc,
	}
	ref := vkAttachmentReference{Attachment: 0, Layout: layoutGeneral}
	// The attachment is written by the pipeline, so it must be in
	// COLOR_ATTACHMENT_OPTIMAL while the subpass runs.
	ref.Layout = 2 /* VK_IMAGE_LAYOUT_COLOR_ATTACHMENT_OPTIMAL */
	sub := vkSubpassDescription{
		PipelineBindPoint:    bindPointGraphics,
		ColorAttachmentCount: 1,
		PColorAttachments:    uintptr(unsafe.Pointer(&ref)),
	}
	// The colour writes must be visible to the transfer stage that copies the
	// frame back to host memory, so declare it explicitly instead of relying on
	// the implicit (weaker) external dependency.
	dep := vkSubpassDependency{
		SrcSubpass: 0, DstSubpass: vkSubpassExternal,
		SrcStageMask: stageColorAttachmentOut, DstStageMask: stageTransfer,
		SrcAccessMask: accessColorAttachmentWrit, DstAccessMask: accessXferRead,
	}
	rp := vkRenderPassCreateInfo{
		SType: stRenderPassCreateInfo, AttachmentCount: 1,
		PAttachments: uintptr(unsafe.Pointer(&attach)),
		SubpassCount: 1, PSubpasses: uintptr(unsafe.Pointer(&sub)),
		DependencyCount: 1, PDependencies: uintptr(unsafe.Pointer(&dep)),
	}
	if rc := v.api.createRenderPass(v.dev, &rp, nil, &v.pass); rc != vkSuccess {
		return fmt.Errorf("vulkan: vkCreateRenderPass failed (VkResult %d)", rc)
	}

	bind := vkDescriptorSetLayoutBinding{
		Binding: 0, DescriptorType: descTypeCombinedImageSampler,
		DescriptorCount: 1, StageFlags: shaderStageFragment,
	}
	dsl := vkDescriptorSetLayoutCreateInfo{
		SType: stDescriptorSetLayoutCreate, BindingCount: 1,
		PBindings: uintptr(unsafe.Pointer(&bind)),
	}
	if rc := v.api.createDescriptorSet(v.dev, &dsl, nil, &v.setLay); rc != vkSuccess {
		return fmt.Errorf("vulkan: vkCreateDescriptorSetLayout failed (VkResult %d)", rc)
	}
	pcr := vkPushConstantRange{StageFlags: shaderStageVertex | shaderStageFragment, Size: uint32(unsafe.Sizeof(pushConstants{}))}
	setLayouts := []uintptr{v.setLay}
	pl := vkPipelineLayoutCreateInfo{
		SType: stPipelineLayoutCreateInfo, SetLayoutCount: 1,
		PSetLayouts:            uintptr(unsafe.Pointer(&setLayouts[0])),
		PushConstantRangeCount: 1,
		PPushConstantRanges:    uintptr(unsafe.Pointer(&pcr)),
	}
	if rc := v.api.createPipelineLayout(v.dev, &pl, nil, &v.pipeLay); rc != vkSuccess {
		return fmt.Errorf("vulkan: vkCreatePipelineLayout failed (VkResult %d)", rc)
	}

	stages := [2]vkPipelineShaderStageCreateInfo{
		{SType: stPipelineShaderStageInfo, Stage: shaderStageVertex, Module: vert, PName: uintptr(unsafe.Pointer(&entryMain[0]))},
		{SType: stPipelineShaderStageInfo, Stage: shaderStageFragment, Module: frag, PName: uintptr(unsafe.Pointer(&entryMain[0]))},
	}
	vi := vkPipelineVertexInputStateCreateInfo{SType: stPipelineVertexInput}
	ia := vkPipelineInputAssemblyStateCreateInfo{SType: stPipelineInputAssembly, Topology: topologyTriangleStrip}
	vp := vkPipelineViewportStateCreateInfo{SType: stPipelineViewportState, ViewportCount: 1, ScissorCount: 1}
	rs := vkPipelineRasterizationStateCreateInfo{
		SType: stPipelineRasterization, PolygonMode: polygonModeFill,
		CullMode: cullModeNone, FrontFace: frontFaceCCW, LineWidth: 1,
	}
	ms := vkPipelineMultisampleStateCreateInfo{SType: stPipelineMultisample, RasterizationSamples: sampleCount1}
	blend := vkPipelineColorBlendAttachmentState{
		BlendEnable:         1,
		SrcColorBlendFactor: blendSrcAlpha, DstColorBlendFactor: blendOneMinusSrcAlpha, ColorBlendOp: blendOpAdd,
		SrcAlphaBlendFactor: blendOne, DstAlphaBlendFactor: blendOneMinusSrcAlpha, AlphaBlendOp: blendOpAdd,
		ColorWriteMask: colorComponentRGBA,
	}
	cb := vkPipelineColorBlendStateCreateInfo{
		SType: stPipelineColorBlend, AttachmentCount: 1,
		PAttachments: uintptr(unsafe.Pointer(&blend)),
	}
	dyn := [2]uint32{dynamicStateViewport, dynamicStateScissor}
	ds := vkPipelineDynamicStateCreateInfo{
		SType: stPipelineDynamicState, DynamicStateCount: 2,
		PDynamicStates: uintptr(unsafe.Pointer(&dyn[0])),
	}
	gp := vkGraphicsPipelineCreateInfo{
		SType: stGraphicsPipelineCreateInfo, StageCount: 2,
		PStages:             uintptr(unsafe.Pointer(&stages[0])),
		PVertexInputState:   uintptr(unsafe.Pointer(&vi)),
		PInputAssemblyState: uintptr(unsafe.Pointer(&ia)),
		PViewportState:      uintptr(unsafe.Pointer(&vp)),
		PRasterizationState: uintptr(unsafe.Pointer(&rs)),
		PMultisampleState:   uintptr(unsafe.Pointer(&ms)),
		PColorBlendState:    uintptr(unsafe.Pointer(&cb)),
		PDynamicState:       uintptr(unsafe.Pointer(&ds)),
		Layout:              v.pipeLay,
		RenderPass:          v.pass,
		BasePipelineIndex:   -1,
	}
	if rc := v.api.createGraphicsPipe(v.dev, 0, 1, &gp, nil, &v.pipeline); rc != vkSuccess {
		return fmt.Errorf("vulkan: vkCreateGraphicsPipelines failed (VkResult %d)", rc)
	}

	si := vkSamplerCreateInfo{
		SType: stSamplerCreateInfo, MagFilter: filterNearest, MinFilter: filterNearest,
		MipmapMode:   mipmapModeNearest,
		AddressModeU: addressModeClampEdge, AddressModeV: addressModeClampEdge, AddressModeW: addressModeClampEdge,
		MaxLod: 0.25, BorderColor: borderColorTransBlack,
	}
	if rc := v.api.createSampler(v.dev, &si, nil, &v.sampler); rc != vkSuccess {
		return fmt.Errorf("vulkan: vkCreateSampler failed (VkResult %d)", rc)
	}
	pool := vkDescriptorPoolCreateInfo{
		SType: stDescriptorPoolCreateInfo, Flags: descPoolFreeDescriptorSet, MaxSets: 4096,
	}
	sizes := [2]vkDescriptorPoolSize{
		{Type: descTypeCombinedImageSampler, DescriptorCount: 4096},
		{Type: 0, DescriptorCount: 0},
	}
	pool.PoolSizeCount = 1
	pool.PPoolSizes = uintptr(unsafe.Pointer(&sizes[0]))
	if rc := v.api.createDescriptorPool(v.dev, &pool, nil, &v.descPool); rc != vkSuccess {
		return fmt.Errorf("vulkan: vkCreateDescriptorPool failed (VkResult %d)", rc)
	}

	white := &shm.Image{Pix: []byte{0xff, 0xff, 0xff, 0xff}, Width: 1, Height: 1, Stride: 4, Format: shm.ARGB8888}
	tex, err := v.newTexture(1, 1)
	if err != nil {
		return err
	}
	tex.white = true
	if err := v.uploadRGBA(tex, white); err != nil {
		return err
	}
	v.white = tex
	return nil
}

var entryMain = []byte("main\x00")

func (v *Vulkan) shaderModule(spv []byte) (uintptr, error) {
	words := spirvWords(spv)
	ci := vkShaderModuleCreateInfo{
		SType:    stShaderModuleCreateInfo,
		CodeSize: uintptr(len(words) * 4),
		PCode:    uintptr(unsafe.Pointer(&words[0])),
	}
	var mod uintptr
	if rc := v.api.createShaderModule(v.dev, &ci, nil, &mod); rc != vkSuccess {
		return 0, fmt.Errorf("vulkan: vkCreateShaderModule failed (VkResult %d)", rc)
	}
	return mod, nil
}

// spirvWords copies SPIR-V bytes into a 4-byte aligned word slice.
func spirvWords(b []byte) []uint32 {
	out := make([]uint32, len(b)/4)
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(b[i*4:])
	}
	return out
}

// ---- per-output resources ----

func (v *Vulkan) ensureTargets(w, h int) error {
	if v.fw == w && v.fh == h && v.outImage != 0 {
		return nil
	}
	v.destroyTargets()
	img, mem, err := v.newImage(w, h, fmtB8G8R8A8UNorm, tilingOptimal, usageColorAttachment|usageTransferSrc|usageTransferDst)
	if err != nil {
		return err
	}
	v.outImage, v.outMem = img, mem
	view, err := v.newImageView(img, fmtB8G8R8A8UNorm)
	if err != nil {
		return err
	}
	v.outView = view
	fb := vkFramebufferCreateInfo{
		SType: stFramebufferCreateInfo, RenderPass: v.pass, AttachmentCount: 1,
		PAttachments: uintptr(unsafe.Pointer(&v.outView)),
		Width:        uint32(w), Height: uint32(h), Layers: 1,
	}
	if rc := v.api.createFramebuffer(v.dev, &fb, nil, &v.framebuf); rc != vkSuccess {
		return fmt.Errorf("vulkan: vkCreateFramebuffer failed (VkResult %d)", rc)
	}
	size := uint64(w * h * 4)
	buf, bmem, err := v.newBuffer(size, usageTransferDst, memHostVisible|memHostCoherent)
	if err != nil {
		return err
	}
	v.readback, v.rbMem, v.rbSize = buf, bmem, size
	if rc := v.api.mapMemory(v.dev, bmem, 0, size, 0, unsafe.Pointer(&v.rbPtr)); rc != vkSuccess {
		return fmt.Errorf("vulkan: vkMapMemory(readback) failed (VkResult %d)", rc)
	}
	v.fw, v.fh = w, h
	return nil
}

func (v *Vulkan) destroyTargets() {
	if v.framebuf != 0 {
		v.api.destroyFramebuffer(v.dev, v.framebuf, nil)
		v.framebuf = 0
	}
	if v.outView != 0 {
		v.api.destroyImageView(v.dev, v.outView, nil)
		v.outView = 0
	}
	if v.outImage != 0 {
		v.api.destroyImage(v.dev, v.outImage, nil)
		v.outImage = 0
	}
	if v.outMem != 0 {
		v.api.freeMemory(v.dev, v.outMem, nil)
		v.outMem = 0
	}
	if v.readback != 0 {
		v.api.unmapMemory(v.dev, v.rbMem)
		v.api.destroyBuffer(v.dev, v.readback, nil)
		v.api.freeMemory(v.dev, v.rbMem, nil)
		v.readback, v.rbMem, v.rbPtr, v.rbSize = 0, 0, nil, 0
	}
	if v.rbPtr != nil {
		v.api.unmapMemory(v.dev, v.rbMem)
		v.rbPtr = nil
	}
	v.fw, v.fh = 0, 0
}

func (v *Vulkan) newImage(w, h int, format, tiling, usage uint32) (uintptr, uintptr, error) {
	ci := vkImageCreateInfo{
		SType: stImageCreateInfo, ImageType: imageType2D, Format: format,
		Extent:    vkExtent3D{Width: int32(w), Height: int32(h), Depth: 1},
		MipLevels: 1, ArrayLayers: 1, Samples: sampleCount1,
		Tiling: tiling, Usage: usage, SharingMode: sharingExclusive,
		InitialLayout: layoutUndefined,
	}
	var img uintptr
	if rc := v.api.createImage(v.dev, &ci, nil, &img); rc != vkSuccess {
		return 0, 0, fmt.Errorf("vulkan: vkCreateImage(%dx%d) failed (VkResult %d)", w, h, rc)
	}
	mem, err := v.bindImage(img)
	if err != nil {
		v.api.destroyImage(v.dev, img, nil)
		return 0, 0, err
	}
	return img, mem, nil
}

func (v *Vulkan) bindImage(img uintptr) (uintptr, error) {
	var req vkMemoryRequirements
	v.api.imageMemReqs(v.dev, img, &req)
	idx, ok := v.findMemoryType(req.MemoryTypeBits, memDeviceLocal)
	if !ok {
		idx, ok = v.findMemoryType(req.MemoryTypeBits, 0)
	}
	if !ok {
		return 0, fmt.Errorf("vulkan: no memory type for image")
	}
	ai := vkMemoryAllocateInfo{SType: stMemoryAllocateInfo, AllocationSize: req.Size, MemoryTypeIndex: idx}
	var mem uintptr
	if rc := v.api.allocateMemory(v.dev, &ai, nil, &mem); rc != vkSuccess {
		return 0, fmt.Errorf("vulkan: vkAllocateMemory(image) failed (VkResult %d)", rc)
	}
	if rc := v.api.bindImageMemory(v.dev, img, mem, 0); rc != vkSuccess {
		v.api.freeMemory(v.dev, mem, nil)
		return 0, fmt.Errorf("vulkan: vkBindImageMemory failed (VkResult %d)", rc)
	}
	return mem, nil
}

func (v *Vulkan) newBuffer(size uint64, usage, props uint32) (uintptr, uintptr, error) {
	ci := vkBufferCreateInfo{
		SType: stBufferCreateInfo, Size: size, Usage: usage, SharingMode: sharingExclusive,
	}
	var buf uintptr
	if rc := v.api.createBuffer(v.dev, &ci, nil, &buf); rc != vkSuccess {
		return 0, 0, fmt.Errorf("vulkan: vkCreateBuffer failed (VkResult %d)", rc)
	}
	var req vkMemoryRequirements
	v.api.bufferMemReqs(v.dev, buf, &req)
	idx, ok := v.findMemoryType(req.MemoryTypeBits, props)
	if !ok {
		v.api.destroyBuffer(v.dev, buf, nil)
		return 0, 0, fmt.Errorf("vulkan: no memory type with flags %#x", props)
	}
	ai := vkMemoryAllocateInfo{SType: stMemoryAllocateInfo, AllocationSize: req.Size, MemoryTypeIndex: idx}
	var mem uintptr
	if rc := v.api.allocateMemory(v.dev, &ai, nil, &mem); rc != vkSuccess {
		v.api.destroyBuffer(v.dev, buf, nil)
		return 0, 0, fmt.Errorf("vulkan: vkAllocateMemory(buffer) failed (VkResult %d)", rc)
	}
	if rc := v.api.bindBufMemory(v.dev, buf, mem, 0); rc != vkSuccess {
		v.api.freeMemory(v.dev, mem, nil)
		v.api.destroyBuffer(v.dev, buf, nil)
		return 0, 0, fmt.Errorf("vulkan: vkBindBufferMemory failed (VkResult %d)", rc)
	}
	return buf, mem, nil
}

func (v *Vulkan) newImageView(img uintptr, format uint32) (uintptr, error) {
	ci := vkImageViewCreateInfo{
		SType: stImageViewCreateInfo, Image: img, ViewType: imageViewType2D, Format: format,
		SubresourceRange: vkImageSubresourceRange{
			AspectMask: aspectColor, BaseMipLevel: 0, LevelCount: 1, BaseArrayLayer: 0, LayerCount: 1,
		},
	}
	var view uintptr
	if rc := v.api.createImageView(v.dev, &ci, nil, &view); rc != vkSuccess {
		return 0, fmt.Errorf("vulkan: vkCreateImageView failed (VkResult %d)", rc)
	}
	return view, nil
}

func (v *Vulkan) findMemoryType(bits, props uint32) (uint32, bool) {
	for i := uint32(0); i < v.memT.MemoryTypeCount; i++ {
		if bits&(1<<i) == 0 {
			continue
		}
		if v.memT.MemoryTypes[i].PropertyFlags&props == props {
			return i, true
		}
	}
	return 0, false
}

// ---- texture upload ----

func (v *Vulkan) textureFor(img *shm.Image) (*vkTexture, error) {
	if t, ok := v.texture[img]; ok && t.w == img.Width && t.h == img.Height {
		return t, nil
	}
	if t, ok := v.texture[img]; ok {
		v.destroyTexture(t)
	}
	t, err := v.newTexture(img.Width, img.Height)
	if err != nil {
		return nil, err
	}
	v.texture[img] = t
	return t, nil
}

func (v *Vulkan) newTexture(w, h int) (*vkTexture, error) {
	img, mem, err := v.newImage(w, h, fmtB8G8R8A8UNorm, tilingOptimal, usageTransferDst|usageSampled)
	if err != nil {
		return nil, err
	}
	view, err := v.newImageView(img, fmtB8G8R8A8UNorm)
	if err != nil {
		v.api.destroyImage(v.dev, img, nil)
		v.api.freeMemory(v.dev, mem, nil)
		return nil, err
	}
	return &vkTexture{image: img, mem: mem, view: view, w: w, h: h, layout: layoutUndefined}, nil
}

func (v *Vulkan) destroyTexture(t *vkTexture) {
	if t == nil {
		return
	}
	if t.view != 0 {
		v.api.destroyImageView(v.dev, t.view, nil)
	}
	if t.image != 0 {
		v.api.destroyImage(v.dev, t.image, nil)
	}
	if t.mem != 0 {
		v.api.freeMemory(v.dev, t.mem, nil)
	}
}

// uploadRGBA creates the texture for a 1x1 white pixel outside a frame (used
// while building the pipeline).
func (v *Vulkan) uploadRGBA(t *vkTexture, img *shm.Image) error {
	cmd := v.cmdBuf
	begin := vkCommandBufferBeginInfo{SType: stCommandBufferBeginInfo}
	v.api.resetCmdBuffer(cmd, 0)
	if rc := v.api.beginCmdBuffer(cmd, &begin); rc != vkSuccess {
		return fmt.Errorf("vulkan: vkBeginCommandBuffer failed (VkResult %d)", rc)
	}
	buf, mem, err := v.newBuffer(uint64(len(img.Pix)), usageTransferSrc, memHostVisible|memHostCoherent)
	if err != nil {
		return err
	}
	defer func() {
		v.api.destroyBuffer(v.dev, buf, nil)
		v.api.freeMemory(v.dev, mem, nil)
	}()
	var p unsafe.Pointer
	if rc := v.api.mapMemory(v.dev, mem, 0, uint64(len(img.Pix)), 0, unsafe.Pointer(&p)); rc != vkSuccess {
		return fmt.Errorf("vulkan: vkMapMemory failed (VkResult %d)", rc)
	}
	copy(unsafe.Slice((*byte)(p), len(img.Pix)), img.Pix)
	v.api.unmapMemory(v.dev, mem)

	v.barrier(cmd, t.image, layoutUndefined, layoutTransferDst, 0, 0, stageTransfer, accessXferWrit)
	region := vkBufferImageCopy{
		ImageSubresource: vkImageSubresourceLayers{AspectMask: aspectColor, LayerCount: 1},
		ImageExtent:      vkExtent3D{Width: int32(img.Width), Height: int32(img.Height), Depth: 1},
	}
	v.api.cmdCopyBufToImg(cmd, buf, t.image, layoutTransferDst, 1, &region)
	v.barrier(cmd, t.image, layoutTransferDst, layoutShaderReadOnly, stageTransfer, accessXferWrit, stageFragmentShader, accessShaderRead)
	t.layout = layoutShaderReadOnly
	if rc := v.api.endCmdBuffer(cmd); rc != vkSuccess {
		return fmt.Errorf("vulkan: vkEndCommandBuffer failed (VkResult %d)", rc)
	}
	return v.submitWait()
}

// barrier records one image layout transition.
func (v *Vulkan) barrier(cmd, img uintptr, old, new, srcStage, srcAccess, dstStage, dstAccess uint32) {
	b := vkImageMemoryBarrier{
		SType: stImageMemoryBarrier, SrcAccessMask: srcAccess, DstAccessMask: dstAccess,
		OldLayout: old, NewLayout: new,
		SrcQueueFamilyIndex: ^uint32(0), DstQueueFamilyIndex: ^uint32(0),
		Image: img,
		SubresourceRange: vkImageSubresourceRange{
			AspectMask: aspectColor, LevelCount: 1, LayerCount: 1,
		},
	}
	v.api.cmdPipelineBar(cmd, srcStage, dstStage, 0, 0, nil, 0, nil, 1, unsafe.Pointer(&b))
}

func (v *Vulkan) submitWait() error {
	cmd := v.cmdBuf
	si := vkSubmitInfo{SType: stSubmitInfo, CommandBufferCount: 1, PCommandBuffers: uintptr(unsafe.Pointer(&cmd))}
	if rc := v.api.queueSubmit(v.queue, 1, &si, v.fence); rc != vkSuccess {
		return fmt.Errorf("vulkan: vkQueueSubmit failed (VkResult %d)", rc)
	}
	fences := [1]uintptr{v.fence}
	if rc := v.api.waitForFences(v.dev, 1, &fences[0], 1, ^uint64(0)); rc != vkSuccess {
		return fmt.Errorf("vulkan: vkWaitForFences failed (VkResult %d)", rc)
	}
	if rc := v.api.resetFences(v.dev, 1, &fences[0]); rc != vkSuccess {
		return fmt.Errorf("vulkan: vkResetFences failed (VkResult %d)", rc)
	}
	return nil
}

// ---- drawing ----

// pushConstants mirrors the std430 layout of the Push block declared in
// shaders/quad.vert and shaders/quad.frag. GLSL and Go disagree here: std430
// makes a vec4 start on a 16-byte boundary, so tint sits at offset 32 and the
// 64-byte block has explicit trailing padding that Go would otherwise omit.
type pushConstants struct {
	Rect    [4]float32 // offset  0: x, y, w, h in output pixels
	OutSize [2]float32 // offset 16: output size in pixels
	_       [2]float32 // offset 24: std430 alignment gap before tint
	Tint    [4]float32 // offset 32: colour multiplier
	Alpha   float32    // offset 48: extra opacity
	_       [3]float32 // offset 52: std430 rounds the block up to 64 bytes
}

type vkDraw struct {
	tex   *vkTexture
	rect  Rect
	tint  [4]float32
	alpha float32
}

// Draw composites items into fb. Items are drawn back-to-front; each item's
// own pixels are uploaded to a device-local image and rasterised with
// source-over blending, which mirrors Framebuffer.ScaleFromImage exactly
// enough that the two renderers agree pixel for pixel.
func (v *Vulkan) Draw(fb *Framebuffer, items []DrawItem) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed || fb.Width <= 0 || fb.Height <= 0 {
		return nil
	}
	if err := v.ensureTargets(fb.Width, fb.Height); err != nil {
		return err
	}

	type upload struct {
		tex    *vkTexture
		img    *shm.Image
		offset uint64
	}
	var (
		uploads []upload
		draws   []vkDraw
		seen    = map[*shm.Image]bool{}
		total   uint64
	)
	alphaFor := func(it *DrawItem) float32 {
		if it.Opacity > 0 && it.Opacity < 1 {
			return float32(it.Opacity)
		}
		return 1
	}
	for i := range items {
		it := &items[i]
		if it.Image == nil || it.W <= 0 || it.H <= 0 {
			continue
		}
		tex, err := v.textureFor(it.Image)
		if err != nil {
			return err
		}
		if !seen[it.Image] {
			seen[it.Image] = true
			uploads = append(uploads, upload{tex: tex, img: it.Image, offset: total})
			total += uint64(it.Image.Stride * it.Image.Height)
		}
		rect := it.IntRect()
		if it.Frame {
			draws = append(draws, v.frameDraws(rect, it.Active)...)
		}
		draws = append(draws, vkDraw{tex: tex, rect: rect, tint: [4]float32{1, 1, 1, 1}, alpha: alphaFor(it)})
		if it.Frame {
			draws = append(draws, v.outlineDraws(rect, it.Active)...)
		}
	}
	if err := v.ensureStaging(total); err != nil {
		return err
	}
	if total > 0 {
		dst := unsafe.Slice((*byte)(v.stgPtr), v.stagingSize)
		for _, u := range uploads {
			// The wl_shm mapping is read-only for us; copy row by row so the
			// staging buffer holds tightly described rows.
			pix := u.img.Pix
			off := u.offset
			opaque := u.img.Format == shm.XRGB8888
			for y := 0; y < u.img.Height; y++ {
				row := pix[y*u.img.Stride:]
				copy(dst[off:], row[:u.img.Stride])
				if opaque {
					// XRGB buffers leave the fourth byte undefined; make them
					// opaque so they composite like the software renderer.
					for x := 0; x < u.img.Width; x++ {
						dst[off+uint64(x*4)+3] = 0xff
					}
				}
				off += uint64(u.img.Stride)
			}
		}
	}

	// Reset per-frame state: the command buffer records one submission and
	// the descriptor pool is recycled, so no ring of frames is needed.
	v.api.resetDescriptorPool(v.dev, v.descPool, 0)
	v.api.resetCmdBuffer(v.cmdBuf, 0)
	begin := vkCommandBufferBeginInfo{SType: stCommandBufferBeginInfo}
	if rc := v.api.beginCmdBuffer(v.cmdBuf, &begin); rc != vkSuccess {
		return fmt.Errorf("vulkan: vkBeginCommandBuffer failed (VkResult %d)", rc)
	}
	// Upload every texture used this frame.
	for _, u := range uploads {
		region := vkBufferImageCopy{
			BufferOffset:     u.offset,
			BufferRowLength:  uint32(u.img.Stride / 4),
			ImageSubresource: vkImageSubresourceLayers{AspectMask: aspectColor, LayerCount: 1},
			ImageExtent:      vkExtent3D{Width: int32(u.img.Width), Height: int32(u.img.Height), Depth: 1},
		}
		srcStage, srcAccess := uint32(stageTopOfPipe), uint32(0)
		if u.tex.layout == layoutShaderReadOnly {
			srcStage, srcAccess = stageFragmentShader, accessShaderRead
		}
		v.barrier(v.cmdBuf, u.tex.image, u.tex.layout, layoutTransferDst, srcStage, srcAccess, stageTransfer, accessXferWrit)
		v.api.cmdCopyBufToImg(v.cmdBuf, v.staging, u.tex.image, layoutTransferDst, 1, &region)
		v.barrier(v.cmdBuf, u.tex.image, layoutTransferDst, layoutShaderReadOnly, stageTransfer, accessXferWrit, stageFragmentShader, accessShaderRead)
		u.tex.layout = layoutShaderReadOnly
	}

	// One descriptor set per texture actually referenced this frame.
	textures := []*vkTexture{v.white}
	index := map[*vkTexture]uintptr{v.white: 0}
	for _, d := range draws {
		if _, ok := index[d.tex]; !ok {
			index[d.tex] = uintptr(len(textures))
			textures = append(textures, d.tex)
		}
	}
	sets := make([]uintptr, len(textures))
	// pSetLayouts must point at an array with one entry per descriptor set
	// requested; a pointer to the single v.setLay field over-reads past the
	// end for frames that reference more than one texture.
	layouts := make([]uintptr, len(textures))
	for i := range layouts {
		layouts[i] = v.setLay
	}
	alloc := vkDescriptorSetAllocateInfo{
		SType: stDescriptorSetAllocateInfo, DescriptorPool: v.descPool,
		DescriptorSetCount: uint32(len(textures)),
		PSetLayouts:        uintptr(unsafe.Pointer(&layouts[0])),
	}
	if rc := v.api.allocDescriptorSets(v.dev, &alloc, &sets[0]); rc != vkSuccess {
		return fmt.Errorf("vulkan: vkAllocateDescriptorSets failed (VkResult %d)", rc)
	}
	writes := make([]vkWriteDescriptorSet, len(textures))
	infos := make([]vkDescriptorImageInfo, len(textures))
	for i, t := range textures {
		infos[i] = vkDescriptorImageInfo{Sampler: v.sampler, ImageView: t.view, ImageLayout: layoutShaderReadOnly}
		writes[i] = vkWriteDescriptorSet{
			SType: stWriteDescriptorSet, DstSet: sets[i], DstBinding: 0,
			DescriptorCount: 1, DescriptorType: descTypeCombinedImageSampler,
			PImageInfo: uintptr(unsafe.Pointer(&infos[i])),
		}
	}
	v.api.updateDescriptorSets(v.dev, uint32(len(writes)), &writes[0], 0, nil)

	// Render pass: clear to the theme background, then blit the quads.
	clear := vkClearColorValue{Float32: rgbaFloats(v.cs.background(v.theme))}
	full := vkRect2D{Extent: vkExtent2D{Width: uint32(fb.Width), Height: uint32(fb.Height)}}
	rbi := vkRenderPassBeginInfo{
		SType: stRenderPassBeginInfo, RenderPass: v.pass, Framebuffer: v.framebuf,
		RenderArea: full, ClearValueCount: 1, PClearValues: uintptr(unsafe.Pointer(&clear)),
	}
	v.api.cmdBeginRenderPass(v.cmdBuf, &rbi, 0)
	v.api.cmdBindPipeline(v.cmdBuf, bindPointGraphics, v.pipeline)
	viewport := vkViewport{X: 0, Y: 0, Width: float32(fb.Width), Height: float32(fb.Height), MinDepth: 0, MaxDepth: 1}
	v.api.cmdSetViewport(v.cmdBuf, 0, 1, &viewport)
	v.api.cmdSetScissor(v.cmdBuf, 0, 1, &full)
	for _, d := range draws {
		set := sets[index[d.tex]]
		v.api.cmdBindDescriptorSet(v.cmdBuf, bindPointGraphics, v.pipeLay, 0, 1, &set)
		pc := pushConstants{
			Rect:    [4]float32{float32(d.rect.X), float32(d.rect.Y), float32(d.rect.W), float32(d.rect.H)},
			OutSize: [2]float32{float32(fb.Width), float32(fb.Height)},
			Tint:    d.tint,
			Alpha:   d.alpha,
		}
		v.api.cmdPushConstants(v.cmdBuf, v.pipeLay, shaderStageVertex|shaderStageFragment, 0, uint32(unsafe.Sizeof(pc)), unsafe.Pointer(&pc))
		v.api.cmdDraw(v.cmdBuf, 4, 1, 0, 0)
	}
	v.api.cmdEndRenderPass(v.cmdBuf)

	// The render pass leaves the image in TRANSFER_SRC_OPTIMAL, so it can be
	// copied straight into the readback buffer.
	copyRegion := vkBufferImageCopy{
		ImageSubresource: vkImageSubresourceLayers{AspectMask: aspectColor, LayerCount: 1},
		ImageExtent:      vkExtent3D{Width: int32(fb.Width), Height: int32(fb.Height), Depth: 1},
	}
	v.api.cmdCopyImgToBuf(v.cmdBuf, v.outImage, layoutTransferSrc, v.readback, 1, &copyRegion)
	if rc := v.api.endCmdBuffer(v.cmdBuf); rc != vkSuccess {
		return fmt.Errorf("vulkan: vkEndCommandBuffer failed (VkResult %d)", rc)
	}
	if err := v.submitWait(); err != nil {
		return err
	}

	// Hand the pixels back in Lumine's XRGB8888 row layout, honouring the
	// destination stride (the DRM dumb buffer can be padded).
	src := unsafe.Slice((*byte)(v.rbPtr), int(v.rbSize))
	for y := 0; y < fb.Height; y++ {
		dstRow := fb.Pix[y*fb.Stride : y*fb.Stride+fb.Width*4]
		copy(dstRow, src[y*fb.Width*4:(y+1)*fb.Width*4])
	}

	// Drop textures whose buffers the client replaced, so the cache stays
	// proportional to the live window set.
	for img, t := range v.texture {
		if !seen[img] {
			v.destroyTexture(t)
			delete(v.texture, img)
		}
	}
	// The transfer function is applied on the CPU side of the readback so the
	// two renderers share one implementation.
	applyColorTransform(fb, v.cs)
	return nil
}

// frameDraws paints the SSD title bar and close affordance above the content.
func (v *Vulkan) frameDraws(r Rect, active bool) []vkDraw {
	bar, close := v.theme.Decoration, uint32(0xffe06060)
	if active {
		bar = v.theme.DecorationActive
	}
	return []vkDraw{
		{tex: v.white, rect: Rect{X: r.X, Y: r.Y - titleBarHeight, W: r.W, H: titleBarHeight}, tint: rgbaFloats(bar), alpha: 1},
		{tex: v.white, rect: Rect{X: r.X + r.W - 20, Y: r.Y - titleBarHeight + 8, W: 12, H: 12}, tint: rgbaFloats(close), alpha: 1},
	}
}

// outlineDraws paints the one-pixel border around the content, on top of it.
func (v *Vulkan) outlineDraws(r Rect, active bool) []vkDraw {
	border := v.theme.Border
	if active {
		border = v.theme.BorderActive
	}
	tint := rgbaFloats(border)
	b := float32(borderWidth)
	_ = b
	return []vkDraw{
		{tex: v.white, rect: Rect{r.X, r.Y, r.W, borderWidth}, tint: tint, alpha: 1},
		{tex: v.white, rect: Rect{r.X, r.Y + r.H - borderWidth, r.W, borderWidth}, tint: tint, alpha: 1},
		{tex: v.white, rect: Rect{r.X, r.Y, borderWidth, r.H}, tint: tint, alpha: 1},
		{tex: v.white, rect: Rect{r.X + r.W - borderWidth, r.Y, borderWidth, r.H}, tint: tint, alpha: 1},
	}
}

func (v *Vulkan) ensureStaging(size uint64) error {
	if size <= v.stagingSize && v.staging != 0 {
		return nil
	}
	if v.staging != 0 {
		v.api.unmapMemory(v.dev, v.stgMem)
		v.api.destroyBuffer(v.dev, v.staging, nil)
		v.api.freeMemory(v.dev, v.stgMem, nil)
		v.staging, v.stgMem, v.stgPtr, v.stagingSize = 0, 0, nil, 0
	}
	if size == 0 {
		return nil
	}
	// Round up so a growing window set does not reallocate every frame.
	size = (size + (1 << 20) - 1) &^ ((1 << 20) - 1)
	buf, mem, err := v.newBuffer(size, usageTransferSrc, memHostVisible|memHostCoherent)
	if err != nil {
		return err
	}
	if rc := v.api.mapMemory(v.dev, mem, 0, size, 0, unsafe.Pointer(&v.stgPtr)); rc != vkSuccess {
		v.api.destroyBuffer(v.dev, buf, nil)
		v.api.freeMemory(v.dev, mem, nil)
		return fmt.Errorf("vulkan: vkMapMemory(staging) failed (VkResult %d)", rc)
	}
	v.staging, v.stgMem, v.stagingSize = buf, mem, size
	return nil
}

// ---- teardown ----

// Close releases every Vulkan object. It is safe to call more than once.
func (v *Vulkan) Close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return
	}
	v.closed = true
	if v.dev != 0 {
		v.api.deviceWaitIdle(v.dev)
	}
	for _, t := range v.texture {
		v.destroyTexture(t)
	}
	v.texture = map[*shm.Image]*vkTexture{}
	if v.white != nil {
		v.destroyTexture(v.white)
		v.white = nil
	}
	if v.staging != 0 {
		v.api.unmapMemory(v.dev, v.stgMem)
		v.api.destroyBuffer(v.dev, v.staging, nil)
		v.api.freeMemory(v.dev, v.stgMem, nil)
		v.staging, v.stgMem, v.stgPtr, v.stagingSize = 0, 0, nil, 0
	}
	v.destroyTargets()
	if v.descPool != 0 {
		v.api.destroyDescriptorPool(v.dev, v.descPool, nil)
	}
	if v.sampler != 0 {
		v.api.destroySampler(v.dev, v.sampler, nil)
	}
	if v.pipeline != 0 {
		v.api.destroyPipeline(v.dev, v.pipeline, nil)
	}
	if v.pipeLay != 0 {
		v.api.destroyPipelineLayout(v.dev, v.pipeLay, nil)
	}
	if v.setLay != 0 {
		v.api.destroyDescriptorSet(v.dev, v.setLay, nil)
	}
	if v.pass != 0 {
		v.api.destroyRenderPass(v.dev, v.pass, nil)
	}
	if v.cmdPool != 0 {
		v.api.destroyCmdPool(v.dev, v.cmdPool, nil)
	}
	if v.fence != 0 {
		v.api.destroyFence(v.dev, v.fence, nil)
	}
	if v.dev != 0 {
		v.api.destroyDevice(v.dev, nil)
	}
	if v.inst != 0 {
		v.api.destroyInstance(v.inst, nil)
	}
	v.dev, v.inst = 0, 0
}

func rgbaFloats(c uint32) [4]float32 {
	return [4]float32{
		float32((c>>16)&0xff) / 255,
		float32((c>>8)&0xff) / 255,
		float32(c&0xff) / 255,
		float32((c>>24)&0xff) / 255,
	}
}
