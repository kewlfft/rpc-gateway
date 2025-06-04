{
  let ptr := mload(0x40)  // Load free memory pointer
  mstore(ptr, gas())      // Store gas left at ptr
  return(ptr, 32)         // Return 32 bytes (gas value)
}
