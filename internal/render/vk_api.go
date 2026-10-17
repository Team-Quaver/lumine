package render

import (
	"fmt"
	"unsafe"

	"github.com/ebitengine/purego"
)

// vkAPI is a dlopen'd libvulkan.so.1. Every entry point is resolved lazily so
// a machine without a Vulkan loader simply falls back to the CPU renderer.
type vkAPI struct {
	lib uintptr

	createInstance    func(*vkInstanceCreateInfo, unsafe.Pointer, *uintptr) int32
	destroyInstance   func(uintptr, unsafe.Pointer)
	enumeratePhysical func(uintptr, *uint32, *uintptr) int32
	getPhysProps      func(uintptr, unsafe.Pointer)
	getQueueFamilies  func(uintptr, *uint32, *vkQueueFamilyProperties)
	getMemProps       func(uintptr, unsafe.Pointer)
	getImageFormat    func(uintptr, uint32, uint32, uint32, uint32, uint32, *vkImageFormatProperties) int32
	createDevice      func(uintptr, *vkDeviceCreateInfo, unsafe.Pointer, *uintptr) int32
	destroyDevice     func(uintptr, unsafe.Pointer)
	getDeviceQueue    func(uintptr, uint32, uint32, *uintptr)
	deviceWaitIdle    func(uintptr) int32

	createImage     func(uintptr, *vkImageCreateInfo, unsafe.Pointer, *uintptr) int32
	destroyImage    func(uintptr, uintptr, unsafe.Pointer)
	imageMemReqs    func(uintptr, uintptr, *vkMemoryRequirements)
	createBuffer    func(uintptr, *vkBufferCreateInfo, unsafe.Pointer, *uintptr) int32
	destroyBuffer   func(uintptr, uintptr, unsafe.Pointer)
	bufferMemReqs   func(uintptr, uintptr, *vkMemoryRequirements)
	allocateMemory  func(uintptr, *vkMemoryAllocateInfo, unsafe.Pointer, *uintptr) int32
	freeMemory      func(uintptr, uintptr, unsafe.Pointer)
	mapMemory       func(uintptr, uintptr, uint64, uint64, uint32, unsafe.Pointer) int32
	unmapMemory     func(uintptr, uintptr)
	bindImageMemory func(uintptr, uintptr, uintptr, uint64) int32
	bindBufMemory   func(uintptr, uintptr, uintptr, uint64) int32

	createCmdPool   func(uintptr, *vkCommandPoolCreateInfo, unsafe.Pointer, *uintptr) int32
	destroyCmdPool  func(uintptr, uintptr, unsafe.Pointer)
	allocCmdBuffers func(uintptr, *vkCommandBufferAllocateInfo, *uintptr) int32
	beginCmdBuffer  func(uintptr, *vkCommandBufferBeginInfo) int32
	endCmdBuffer    func(uintptr) int32
	resetCmdBuffer  func(uintptr, uint32) int32
	queueSubmit     func(uintptr, uint32, *vkSubmitInfo, uintptr) int32
	queueWaitIdle   func(uintptr) int32
	createFence     func(uintptr, *vkFenceCreateInfo, unsafe.Pointer, *uintptr) int32
	destroyFence    func(uintptr, uintptr, unsafe.Pointer)
	waitForFences   func(uintptr, uint32, *uintptr, uint32, uint64) int32
	resetFences     func(uintptr, uint32, *uintptr) int32
	cmdPipelineBar  func(uintptr, uint32, uint32, uint32, uint32, unsafe.Pointer, uint32, unsafe.Pointer, uint32, unsafe.Pointer)
	cmdClearColor   func(uintptr, uintptr, uint32, *vkClearColorValue, uint32, *vkImageSubresourceRange)
	cmdBlitImage    func(uintptr, uintptr, uint32, uintptr, uint32, uint32, *vkImageBlit, uint32)
	cmdCopyBufToImg func(uintptr, uintptr, uintptr, uint32, uint32, *vkBufferImageCopy)
	cmdCopyImgToBuf func(uintptr, uintptr, uint32, uintptr, uint32, *vkBufferImageCopy)
	cmdCopyBuffer   func(uintptr, uintptr, uintptr, uint32, *vkCopyBuffer)

	// pipeline / render pass
	createShaderModule    func(uintptr, *vkShaderModuleCreateInfo, unsafe.Pointer, *uintptr) int32
	destroyShaderModule   func(uintptr, uintptr, unsafe.Pointer)
	createDescriptorSet   func(uintptr, *vkDescriptorSetLayoutCreateInfo, unsafe.Pointer, *uintptr) int32
	destroyDescriptorSet  func(uintptr, uintptr, unsafe.Pointer)
	createPipelineLayout  func(uintptr, *vkPipelineLayoutCreateInfo, unsafe.Pointer, *uintptr) int32
	destroyPipelineLayout func(uintptr, uintptr, unsafe.Pointer)
	createRenderPass      func(uintptr, *vkRenderPassCreateInfo, unsafe.Pointer, *uintptr) int32
	destroyRenderPass     func(uintptr, uintptr, unsafe.Pointer)
	createGraphicsPipe    func(uintptr, uintptr, uint32, *vkGraphicsPipelineCreateInfo, unsafe.Pointer, *uintptr) int32
	destroyPipeline       func(uintptr, uintptr, unsafe.Pointer)
	createImageView       func(uintptr, *vkImageViewCreateInfo, unsafe.Pointer, *uintptr) int32
	destroyImageView      func(uintptr, uintptr, unsafe.Pointer)
	createFramebuffer     func(uintptr, *vkFramebufferCreateInfo, unsafe.Pointer, *uintptr) int32
	destroyFramebuffer    func(uintptr, uintptr, unsafe.Pointer)
	createSampler         func(uintptr, *vkSamplerCreateInfo, unsafe.Pointer, *uintptr) int32
	destroySampler        func(uintptr, uintptr, unsafe.Pointer)
	createDescriptorPool  func(uintptr, *vkDescriptorPoolCreateInfo, unsafe.Pointer, *uintptr) int32
	destroyDescriptorPool func(uintptr, uintptr, unsafe.Pointer)
	allocDescriptorSets   func(uintptr, *vkDescriptorSetAllocateInfo, *uintptr) int32
	resetDescriptorPool   func(uintptr, uintptr, uint32) int32
	updateDescriptorSets  func(uintptr, uint32, *vkWriteDescriptorSet, uint32, unsafe.Pointer)

	cmdBeginRenderPass   func(uintptr, *vkRenderPassBeginInfo, uint32)
	cmdEndRenderPass     func(uintptr)
	cmdBindPipeline      func(uintptr, uint32, uintptr)
	cmdSetViewport       func(uintptr, uint32, uint32, *vkViewport)
	cmdSetScissor        func(uintptr, uint32, uint32, *vkRect2D)
	cmdBindDescriptorSet func(uintptr, uint32, uintptr, uint32, uint32, *uintptr)
	cmdPushConstants     func(uintptr, uintptr, uint32, uint32, uint32, unsafe.Pointer)
	cmdDraw              func(uintptr, uint32, uint32, uint32, uint32)
}

// vkQueueFamilyProperties mirrors VkQueueFamilyProperties exactly: gcc
// reports sizeof == 24 with minImageTransferGranularity at offset 12. The
// driver writes a tightly packed array of these, so an extra member would
// shift every following element.
type vkQueueFamilyProperties struct {
	QueueFlags                  uint32
	QueueCount                  uint32
	TimestampValidBits          uint32
	MinImageTransferGranularity vkExtent3D
}

// loadVulkan dlopens the Vulkan loader and binds the entry points we use.
func loadVulkan() (*vkAPI, error) {
	var lib uintptr
	var err error
	for _, name := range []string{"libvulkan.so.1", "libvulkan.so"} {
		lib, err = purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err == nil {
			break
		}
	}
	if lib == 0 {
		return nil, fmt.Errorf("vulkan: no loader: %w", err)
	}
	a := &vkAPI{lib: lib}
	binds := []struct {
		dst  any
		name string
	}{
		{&a.createInstance, "vkCreateInstance"},
		{&a.destroyInstance, "vkDestroyInstance"},
		{&a.enumeratePhysical, "vkEnumeratePhysicalDevices"},
		{&a.getPhysProps, "vkGetPhysicalDeviceProperties"},
		{&a.getQueueFamilies, "vkGetPhysicalDeviceQueueFamilyProperties"},
		{&a.getMemProps, "vkGetPhysicalDeviceMemoryProperties"},
		{&a.getImageFormat, "vkGetPhysicalDeviceImageFormatProperties"},
		{&a.createDevice, "vkCreateDevice"},
		{&a.destroyDevice, "vkDestroyDevice"},
		{&a.getDeviceQueue, "vkGetDeviceQueue"},
		{&a.deviceWaitIdle, "vkDeviceWaitIdle"},
		{&a.createImage, "vkCreateImage"},
		{&a.destroyImage, "vkDestroyImage"},
		{&a.imageMemReqs, "vkGetImageMemoryRequirements"},
		{&a.createBuffer, "vkCreateBuffer"},
		{&a.destroyBuffer, "vkDestroyBuffer"},
		{&a.bufferMemReqs, "vkGetBufferMemoryRequirements"},
		{&a.allocateMemory, "vkAllocateMemory"},
		{&a.freeMemory, "vkFreeMemory"},
		{&a.mapMemory, "vkMapMemory"},
		{&a.unmapMemory, "vkUnmapMemory"},
		{&a.bindImageMemory, "vkBindImageMemory"},
		{&a.bindBufMemory, "vkBindBufferMemory"},
		{&a.createCmdPool, "vkCreateCommandPool"},
		{&a.destroyCmdPool, "vkDestroyCommandPool"},
		{&a.allocCmdBuffers, "vkAllocateCommandBuffers"},
		{&a.beginCmdBuffer, "vkBeginCommandBuffer"},
		{&a.endCmdBuffer, "vkEndCommandBuffer"},
		{&a.resetCmdBuffer, "vkResetCommandBuffer"},
		{&a.queueSubmit, "vkQueueSubmit"},
		{&a.queueWaitIdle, "vkQueueWaitIdle"},
		{&a.createFence, "vkCreateFence"},
		{&a.destroyFence, "vkDestroyFence"},
		{&a.waitForFences, "vkWaitForFences"},
		{&a.resetFences, "vkResetFences"},
		{&a.cmdPipelineBar, "vkCmdPipelineBarrier"},
		{&a.cmdClearColor, "vkCmdClearColorImage"},
		{&a.cmdBlitImage, "vkCmdBlitImage"},
		{&a.cmdCopyBufToImg, "vkCmdCopyBufferToImage"},
		{&a.cmdCopyImgToBuf, "vkCmdCopyImageToBuffer"},
		{&a.cmdCopyBuffer, "vkCmdCopyBuffer"},
		{&a.createShaderModule, "vkCreateShaderModule"},
		{&a.destroyShaderModule, "vkDestroyShaderModule"},
		{&a.createDescriptorSet, "vkCreateDescriptorSetLayout"},
		{&a.destroyDescriptorSet, "vkDestroyDescriptorSetLayout"},
		{&a.createPipelineLayout, "vkCreatePipelineLayout"},
		{&a.destroyPipelineLayout, "vkDestroyPipelineLayout"},
		{&a.createRenderPass, "vkCreateRenderPass"},
		{&a.destroyRenderPass, "vkDestroyRenderPass"},
		{&a.createGraphicsPipe, "vkCreateGraphicsPipelines"},
		{&a.destroyPipeline, "vkDestroyPipeline"},
		{&a.createImageView, "vkCreateImageView"},
		{&a.destroyImageView, "vkDestroyImageView"},
		{&a.createFramebuffer, "vkCreateFramebuffer"},
		{&a.destroyFramebuffer, "vkDestroyFramebuffer"},
		{&a.createSampler, "vkCreateSampler"},
		{&a.destroySampler, "vkDestroySampler"},
		{&a.createDescriptorPool, "vkCreateDescriptorPool"},
		{&a.destroyDescriptorPool, "vkDestroyDescriptorPool"},
		{&a.allocDescriptorSets, "vkAllocateDescriptorSets"},
		{&a.resetDescriptorPool, "vkResetDescriptorPool"},
		{&a.updateDescriptorSets, "vkUpdateDescriptorSets"},
		{&a.cmdBeginRenderPass, "vkCmdBeginRenderPass"},
		{&a.cmdEndRenderPass, "vkCmdEndRenderPass"},
		{&a.cmdBindPipeline, "vkCmdBindPipeline"},
		{&a.cmdSetViewport, "vkCmdSetViewport"},
		{&a.cmdSetScissor, "vkCmdSetScissor"},
		{&a.cmdBindDescriptorSet, "vkCmdBindDescriptorSets"},
		{&a.cmdPushConstants, "vkCmdPushConstants"},
		{&a.cmdDraw, "vkCmdDraw"},
	}
	for _, b := range binds {
		purego.RegisterLibFunc(b.dst, lib, b.name)
	}
	return a, nil
}
